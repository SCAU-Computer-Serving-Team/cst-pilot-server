// 发行包分发 · 原子存储、崩溃自愈、历史版本配额回收与审计日志。
//
// 发行包目录与遥测 SQLite 物理隔离，热路径通过不可变内存快照无锁响应。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	releaseStateFilename = "state.json"
	releaseLockFilename  = ".lock"
	releaseAuditFilename = "audit.jsonl"
	releaseTmpDirName    = ".tmp"
	releaseVersionsDir   = "versions"
	releaseAuditMaxBytes = 5 << 20 // 5 MiB 轮转上限
)

var (
	errReleaseBusy         = errors.New("publish_in_progress")
	errVersionExists       = errors.New("version_already_exists")
	errVersionNotFound     = errors.New("release_not_found")
	errVersionActive       = errors.New("version_active")
	errInvalidChannel      = errors.New("invalid_channel")
	errInsufficientStorage = errors.New("insufficient_storage")
	errArchiveTooLarge     = errors.New("archive_too_large")
	errVerifyFailed        = errors.New("verification_failed")
)

type releaseError struct {
	kind error
	msg  string
}

func (e *releaseError) Error() string { return e.msg }
func (e *releaseError) Unwrap() error { return e.kind }

func wrapReleaseErr(kind error, format string, args ...any) error {
	return &releaseError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

type releaseState struct {
	SchemaVersion  int                       `json:"schemaVersion"`
	Channels       map[string]string         `json:"channels"`
	PreviousStable string                    `json:"previousStable,omitempty"`
	Versions       map[string]*releaseRecord `json:"versions"`
}

type releaseRecord struct {
	Version              string `json:"version"`
	Filename             string `json:"filename"`
	ArchiveSize          int64  `json:"archiveSize"`
	UnpackedSize         uint64 `json:"unpackedSize"`
	ArchiveSHA256        string `json:"archiveSha256"`
	ManifestSHA256       string `json:"manifestSha256"`
	BuiltAt              string `json:"builtAt"`
	PublishedAt          string `json:"publishedAt"`
	MinUpgradableVersion string `json:"minUpgradableVersion"`
}

type channelSnapshot struct {
	Version        string
	ManifestBytes  []byte
	ManifestSHA256 string
	PublishedAt    string
}

type fileSnapshot struct {
	Version       string
	Filename      string
	ArchivePath   string
	ArchiveSize   int64
	ArchiveSHA256 string
	PublishedAt   string
}

type releaseHealthSummary struct {
	Enabled        bool   `json:"enabled"`
	StableVersion  string `json:"stableVersion"`
	BetaVersion    string `json:"betaVersion"`
	StoredVersions int    `json:"storedVersions"`
	TotalBytes     int64  `json:"totalBytes"`
	FreeBytes      uint64 `json:"freeBytes"`
	BudgetTripped  bool   `json:"budgetTripped"`
}

type releaseSnapshot struct {
	channels       map[string]*channelSnapshot
	files          map[string]*fileSnapshot
	previousStable string
	records        []*releaseRecord
	totalBytes     int64
}

type releaseAuditEntry struct {
	At      string         `json:"at"`
	Action  string         `json:"action"`
	Actor   string         `json:"actor"`
	Channel string         `json:"channel,omitempty"`
	Version string         `json:"version,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

type publishResult struct {
	Record     *releaseRecord `json:"record"`
	Channel    string         `json:"channel"`
	Activated  bool           `json:"activated"`
	Evicted    []string       `json:"evicted"`
	FreeBytes  uint64         `json:"freeBytes"`
	TotalBytes int64          `json:"totalBytes"`
}

type releaseStore struct {
	cfg       releaseConfig
	now       func() time.Time
	freeBytes func(string) (uint64, error)

	writeBusy atomic.Bool
	stateMu   sync.Mutex
	state     *releaseState
	snapshot  atomic.Pointer[releaseSnapshot]
}

func openReleaseStore(cfg releaseConfig) (*releaseStore, error) {
	return openReleaseStoreWithHooks(cfg, time.Now, availableDiskBytes)
}

func openReleaseStoreWithHooks(cfg releaseConfig, nowFn func() time.Time, freeFn func(string) (uint64, error)) (*releaseStore, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := validateReleaseConfig(cfg); err != nil {
		return nil, err
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	if freeFn == nil {
		freeFn = availableDiskBytes
	}
	rs := &releaseStore{
		cfg:       cfg,
		now:       nowFn,
		freeBytes: freeFn,
	}
	if err := rs.initAndRecover(); err != nil {
		return nil, err
	}
	return rs, nil
}

func (s *releaseStore) tmpDir() string { return filepath.Join(s.cfg.StorageDir, releaseTmpDirName) }
func (s *releaseStore) versionsDir() string {
	return filepath.Join(s.cfg.StorageDir, releaseVersionsDir)
}
func (s *releaseStore) statePath() string {
	return filepath.Join(s.cfg.StorageDir, releaseStateFilename)
}
func (s *releaseStore) lockPath() string { return filepath.Join(s.cfg.StorageDir, releaseLockFilename) }
func (s *releaseStore) auditPath() string {
	return filepath.Join(s.cfg.StorageDir, releaseAuditFilename)
}

func (s *releaseStore) versionDir(version string) string {
	return filepath.Join(s.versionsDir(), version)
}

func ensureNonSymlinkDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path %s must be a real directory", path)
	}
	return os.Chmod(path, perm)
}

func (s *releaseStore) acquireExclusiveLock() (func(), error) {
	if !s.writeBusy.CompareAndSwap(false, true) {
		return nil, wrapReleaseErr(errReleaseBusy, "another release mutation is in progress")
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		s.writeBusy.Store(false)
		return nil, err
	}
	locked, err := tryLockFile(f)
	if err != nil {
		f.Close()
		s.writeBusy.Store(false)
		return nil, err
	}
	if !locked {
		f.Close()
		s.writeBusy.Store(false)
		return nil, wrapReleaseErr(errReleaseBusy, "another process holds the release lock")
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
		s.writeBusy.Store(false)
	}, nil
}

func (s *releaseStore) initAndRecover() error {
	if err := ensureNonSymlinkDir(s.cfg.StorageDir, 0o755); err != nil {
		return fmt.Errorf("init release storage root: %w", err)
	}
	if err := ensureNonSymlinkDir(s.tmpDir(), 0o700); err != nil {
		return fmt.Errorf("init release tmp dir: %w", err)
	}
	if err := ensureNonSymlinkDir(s.versionsDir(), 0o755); err != nil {
		return fmt.Errorf("init release versions dir: %w", err)
	}

	unlock, err := s.acquireExclusiveLock()
	if err != nil {
		return fmt.Errorf("lock release storage on startup: %w", err)
	}
	defer unlock()

	// 清理残留未完成上传与临时状态文件。
	if err := cleanDirContentsNoSymlink(s.tmpDir()); err != nil {
		return fmt.Errorf("cleanup release tmp dir: %w", err)
	}

	state, err := s.loadStateFile()
	if err != nil {
		return err
	}

	// 扫描 versions/ 目录，移除未登记在 state.json 中的孤儿目录，并拒绝符号链接。
	entries, err := os.ReadDir(s.versionsDir())
	if err != nil {
		return fmt.Errorf("scan release versions dir: %w", err)
	}
	for _, entry := range entries {
		full := filepath.Join(s.versionsDir(), entry.Name())
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("stat version entry %s: %w", entry.Name(), err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected in release versions directory: %s", entry.Name())
		}
		if _, known := state.Versions[entry.Name()]; !known {
			fmt.Fprintf(os.Stderr, "release recovery: removing orphan entry %s\n", entry.Name())
			if err := safeRemoveAllNoSymlink(full); err != nil {
				return fmt.Errorf("remove orphan release %s: %w", entry.Name(), err)
			}
		}
	}

	// 核验所有已登记版本，激活版本受损时直接阻断启动。
	activeVersions := map[string]bool{}
	for _, ch := range []string{"stable", "beta"} {
		if v := state.Channels[ch]; v != "" {
			activeVersions[v] = true
		}
	}
	for ver, rec := range state.Versions {
		if !isValidReleaseVersionString(ver) || rec == nil || rec.Version != ver {
			return fmt.Errorf("corrupt state entry for version %q", ver)
		}
		if _, err := s.verifyStoredVersionOnDisk(rec); err != nil {
			if activeVersions[ver] {
				return fmt.Errorf("active release %s verification failed: %w", ver, err)
			}
			fmt.Fprintf(os.Stderr, "release recovery: inactive version %s invalid: %v\n", ver, err)
			delete(state.Versions, ver)
			if state.PreviousStable == ver {
				state.PreviousStable = ""
			}
			_ = safeRemoveAllNoSymlink(s.versionDir(ver))
		}
	}
	for _, ch := range []string{"stable", "beta"} {
		if v := state.Channels[ch]; v != "" {
			if _, ok := state.Versions[v]; !ok {
				return fmt.Errorf("active channel %s points to missing version %s", ch, v)
			}
		}
	}

	snap, err := s.buildSnapshotLocked(state)
	if err != nil {
		return err
	}
	if err := s.saveStateLocked(state); err != nil {
		return err
	}
	s.stateMu.Lock()
	s.state = state
	s.stateMu.Unlock()
	s.snapshot.Store(snap)
	return nil
}

func (s *releaseStore) loadStateFile() (*releaseState, error) {
	raw, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return &releaseState{
			SchemaVersion: 1,
			Channels:      map[string]string{"stable": "", "beta": ""},
			Versions:      map[string]*releaseRecord{},
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release state: %w", err)
	}
	var st releaseState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse release state: %w", err)
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = 1
	}
	if st.Channels == nil {
		st.Channels = map[string]string{"stable": "", "beta": ""}
	}
	if _, ok := st.Channels["stable"]; !ok {
		st.Channels["stable"] = ""
	}
	if _, ok := st.Channels["beta"]; !ok {
		st.Channels["beta"] = ""
	}
	if st.Versions == nil {
		st.Versions = map[string]*releaseRecord{}
	}
	return &st, nil
}

func (s *releaseStore) saveStateLocked(st *releaseState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmpName := filepath.Join(s.tmpDir(), "state-"+randomHex(8)+".json")
	f, err := os.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.statePath()); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return syncDir(s.cfg.StorageDir)
}

func (s *releaseStore) verifyStoredVersionOnDisk(rec *releaseRecord) ([]byte, error) {
	vDir := s.versionDir(rec.Version)
	info, err := os.Lstat(vDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("version directory invalid")
	}
	manifestPath := filepath.Join(vDir, "manifest.json")
	mInfo, err := os.Lstat(manifestPath)
	if err != nil || !mInfo.Mode().IsRegular() || mInfo.Size() > maxManifestBytes {
		return nil, fmt.Errorf("manifest file invalid")
	}
	envBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	verified, err := verifyReleaseManifestEnvelope(envBytes, s.cfg.PublicKey, s.cfg.PublicBaseURL, s.cfg.MaxArchiveBytes)
	if err != nil {
		return nil, err
	}
	if verified.Info.Version != rec.Version || verified.Filename != rec.Filename {
		return nil, fmt.Errorf("manifest version or filename mismatch")
	}
	archivePath := filepath.Join(vDir, rec.Filename)
	aInfo, err := os.Lstat(archivePath)
	if err != nil || !aInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("archive file missing or irregular")
	}
	if aInfo.Size() != rec.ArchiveSize || uint64(aInfo.Size()) != verified.Info.Archive.Size {
		return nil, fmt.Errorf("archive file size mismatch on disk")
	}
	return envBytes, nil
}

func (s *releaseStore) buildSnapshotLocked(st *releaseState) (*releaseSnapshot, error) {
	snap := &releaseSnapshot{
		channels:       make(map[string]*channelSnapshot, 2),
		files:          make(map[string]*fileSnapshot, len(st.Versions)),
		previousStable: st.PreviousStable,
		records:        make([]*releaseRecord, 0, len(st.Versions)),
	}
	for ver, rec := range st.Versions {
		recCopy := *rec
		snap.records = append(snap.records, &recCopy)
		snap.totalBytes += rec.ArchiveSize
		snap.files[ver] = &fileSnapshot{
			Version:       rec.Version,
			Filename:      rec.Filename,
			ArchivePath:   filepath.Join(s.versionDir(rec.Version), rec.Filename),
			ArchiveSize:   rec.ArchiveSize,
			ArchiveSHA256: rec.ArchiveSHA256,
			PublishedAt:   rec.PublishedAt,
		}
	}
	sort.Slice(snap.records, func(i, j int) bool {
		if snap.records[i].PublishedAt != snap.records[j].PublishedAt {
			return snap.records[i].PublishedAt > snap.records[j].PublishedAt
		}
		return snap.records[i].Version > snap.records[j].Version
	})
	for _, ch := range []string{"stable", "beta"} {
		ver := st.Channels[ch]
		if ver == "" {
			continue
		}
		rec := st.Versions[ver]
		if rec == nil {
			continue
		}
		envBytes, err := os.ReadFile(filepath.Join(s.versionDir(ver), "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("load active manifest %s/%s: %w", ch, ver, err)
		}
		snap.channels[ch] = &channelSnapshot{
			Version:        ver,
			ManifestBytes:  envBytes,
			ManifestSHA256: rec.ManifestSHA256,
			PublishedAt:    rec.PublishedAt,
		}
	}
	return snap, nil
}

func (s *releaseStore) currentSnapshot() *releaseSnapshot {
	return s.snapshot.Load()
}

func (s *releaseStore) getChannel(channel string) (*channelSnapshot, bool) {
	snap := s.currentSnapshot()
	if snap == nil {
		return nil, false
	}
	ch, ok := snap.channels[channel]
	return ch, ok
}

func (s *releaseStore) getFile(version, filename string) (*fileSnapshot, bool) {
	snap := s.currentSnapshot()
	if snap == nil {
		return nil, false
	}
	f, ok := snap.files[version]
	if !ok || f.Filename != filename {
		return nil, false
	}
	return f, true
}

func (s *releaseStore) healthSummary(budgetTripped bool) releaseHealthSummary {
	snap := s.currentSnapshot()
	summary := releaseHealthSummary{
		Enabled:       true,
		BudgetTripped: budgetTripped,
	}
	if snap != nil {
		if st, ok := snap.channels["stable"]; ok {
			summary.StableVersion = st.Version
		}
		if bt, ok := snap.channels["beta"]; ok {
			summary.BetaVersion = bt.Version
		}
		summary.StoredVersions = len(snap.records)
		summary.TotalBytes = snap.totalBytes
	}
	if free, err := s.freeBytes(s.cfg.StorageDir); err == nil {
		summary.FreeBytes = free
	}
	return summary
}

func (s *releaseStore) inventorySnapshot() map[string]any {
	snap := s.currentSnapshot()
	channels := map[string]string{"stable": "", "beta": ""}
	records := []*releaseRecord{}
	var totalBytes int64
	previousStable := ""
	if snap != nil {
		if st, ok := snap.channels["stable"]; ok {
			channels["stable"] = st.Version
		}
		if bt, ok := snap.channels["beta"]; ok {
			channels["beta"] = bt.Version
		}
		records = snap.records
		totalBytes = snap.totalBytes
		previousStable = snap.previousStable
	}
	free, _ := s.freeBytes(s.cfg.StorageDir)
	return map[string]any{
		"channels":       channels,
		"previousStable": previousStable,
		"versions":       records,
		"storage": map[string]any{
			"totalBytes":      totalBytes,
			"maxTotalBytes":   s.cfg.MaxTotalBytes,
			"storedVersions":  len(records),
			"keepCount":       s.cfg.KeepCount,
			"freeBytes":       free,
			"minFreeBytes":    s.cfg.MinFreeBytes,
			"maxArchiveBytes": s.cfg.MaxArchiveBytes,
			"accelRedirect":   s.cfg.AccelPrefix != "",
			"publicBaseUrl":   s.cfg.PublicBaseURL,
		},
	}
}

// publishStreaming 执行完整原子发布状态机：
// 拿锁 -> 验签 manifest -> 查重 -> 磁盘水位预检 -> 通过 nextArchive 获取流并落盘算哈希 -> 流式 ZIP 全树核验 -> 原子移入并更新 state/快照 -> GC。
func (s *releaseStore) publishStreaming(
	channel string,
	activate bool,
	manifestBytes []byte,
	nextArchive func() (io.Reader, error),
	actor string,
) (*publishResult, error) {
	if !isValidReleaseChannel(channel) {
		return nil, wrapReleaseErr(errInvalidChannel, "channel must be stable or beta")
	}
	unlock, err := s.acquireExclusiveLock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	verified, err := verifyReleaseManifestEnvelope(manifestBytes, s.cfg.PublicKey, s.cfg.PublicBaseURL, s.cfg.MaxArchiveBytes)
	if err != nil {
		return nil, wrapReleaseErr(errVerifyFailed, "%v", err)
	}
	ver := verified.Info.Version

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	if _, exists := curState.Versions[ver]; exists {
		return nil, wrapReleaseErr(errVersionExists, "version %s already exists and is immutable", ver)
	}
	if _, err := os.Lstat(s.versionDir(ver)); err == nil {
		return nil, wrapReleaseErr(errVersionExists, "version directory %s already exists", ver)
	}

	// 磁盘空间水位预检（在接收大包前拦截）。
	requiredFree := requiredDiskBytesForArchive(verified.Info.Archive.Size, s.cfg.MinFreeBytes)
	freeNow, err := s.freeBytes(s.cfg.StorageDir)
	if err != nil {
		return nil, fmt.Errorf("check disk free space: %w", err)
	}
	if freeNow < requiredFree {
		return nil, wrapReleaseErr(errInsufficientStorage, "insufficient disk space: free %d bytes, required %d bytes", freeNow, requiredFree)
	}

	archiveReader, err := nextArchive()
	if err != nil {
		return nil, err
	}

	stageDir := filepath.Join(s.tmpDir(), "upload-"+randomHex(8))
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return nil, fmt.Errorf("create upload stage dir: %w", err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = safeRemoveAllNoSymlink(stageDir)
		}
	}()

	archiveStagePath := filepath.Join(stageDir, verified.Filename)
	archiveFile, err := os.OpenFile(archiveStagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create temp archive file: %w", err)
	}

	// 流式落盘并同步计算 SHA-256。
	limit := int64(verified.Info.Archive.Size)
	if s.cfg.MaxArchiveBytes < limit {
		limit = s.cfg.MaxArchiveBytes
	}
	hasher := sha256.New()
	bufPtr := releaseBufPool.Get().(*[]byte)
	written, copyErr := io.CopyBuffer(io.MultiWriter(archiveFile, hasher), io.LimitReader(archiveReader, limit+1), *bufPtr)
	releaseBufPool.Put(bufPtr)
	if copyErr != nil {
		archiveFile.Close()
		return nil, fmt.Errorf("stream archive upload: %w", copyErr)
	}
	if written > limit {
		archiveFile.Close()
		return nil, wrapReleaseErr(errArchiveTooLarge, "archive exceeds declared or configured size limit")
	}
	if uint64(written) != verified.Info.Archive.Size {
		archiveFile.Close()
		return nil, wrapReleaseErr(errVerifyFailed, "archive size mismatch: uploaded %d want %d", written, verified.Info.Archive.Size)
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA256 != verified.Info.Archive.SHA256 {
		archiveFile.Close()
		return nil, wrapReleaseErr(errVerifyFailed, "archive sha256 mismatch")
	}
	if err := archiveFile.Sync(); err != nil {
		archiveFile.Close()
		return nil, fmt.Errorf("fsync archive: %w", err)
	}

	// 零落盘流式校验 ZIP 中央目录与全树内容。
	if err := verifyReleaseArchiveStreaming(archiveFile, written, verified); err != nil {
		archiveFile.Close()
		return nil, wrapReleaseErr(errVerifyFailed, "%v", err)
	}
	if err := archiveFile.Close(); err != nil {
		return nil, fmt.Errorf("close archive: %w", err)
	}

	manifestStagePath := filepath.Join(stageDir, "manifest.json")
	if err := writeFileSync(manifestStagePath, verified.Envelope, 0o644); err != nil {
		return nil, fmt.Errorf("write manifest file: %w", err)
	}
	if err := os.Chmod(archiveStagePath, 0o644); err != nil {
		return nil, fmt.Errorf("chmod archive: %w", err)
	}
	if err := os.Chmod(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("chmod version stage dir: %w", err)
	}

	targetDir := s.versionDir(ver)
	if err := os.Rename(stageDir, targetDir); err != nil {
		return nil, fmt.Errorf("atomic rename version dir: %w", err)
	}
	cleanupStage = false
	if err := syncDir(s.versionsDir()); err != nil {
		return nil, fmt.Errorf("fsync versions dir: %w", err)
	}

	nowStr := utcNow(s.now())
	rec := &releaseRecord{
		Version:              ver,
		Filename:             verified.Filename,
		ArchiveSize:          written,
		UnpackedSize:         verified.Info.Archive.UnpackedSize,
		ArchiveSHA256:        actualSHA256,
		ManifestSHA256:       verified.ManifestSHA256,
		BuiltAt:              verified.Info.BuiltAt,
		PublishedAt:          nowStr,
		MinUpgradableVersion: verified.Info.MinUpgradableVersion,
	}
	curState.Versions[ver] = rec
	if activate {
		if channel == "stable" && curState.Channels["stable"] != "" && curState.Channels["stable"] != ver {
			curState.PreviousStable = curState.Channels["stable"]
		}
		curState.Channels[channel] = ver
	}

	evicted, err := s.commitAndGCLocked(curState)
	if err != nil {
		return nil, err
	}

	s.appendAudit(releaseAuditEntry{
		At:      nowStr,
		Action:  "publish",
		Actor:   actor,
		Channel: channel,
		Version: ver,
		Detail: map[string]any{
			"activated":   activate,
			"archiveSize": written,
			"sha256":      actualSHA256,
			"evicted":     evicted,
		},
	})

	freeAfter, _ := s.freeBytes(s.cfg.StorageDir)
	snap := s.currentSnapshot()
	var totalBytes int64
	if snap != nil {
		totalBytes = snap.totalBytes
	}
	recCopy := *rec
	return &publishResult{
		Record:     &recCopy,
		Channel:    channel,
		Activated:  activate,
		Evicted:    evicted,
		FreeBytes:  freeAfter,
		TotalBytes: totalBytes,
	}, nil
}

func (s *releaseStore) activateChannel(channel, version, actor string) (*releaseRecord, error) {
	if !isValidReleaseChannel(channel) {
		return nil, wrapReleaseErr(errInvalidChannel, "channel must be stable or beta")
	}
	if !isValidReleaseVersionString(version) {
		return nil, wrapReleaseErr(errVersionNotFound, "invalid version format")
	}
	unlock, err := s.acquireExclusiveLock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	rec, ok := curState.Versions[version]
	if !ok {
		return nil, wrapReleaseErr(errVersionNotFound, "version %s is not stored", version)
	}
	// 切换前二次复验磁盘签名与归档体积。
	if _, err := s.verifyStoredVersionOnDisk(rec); err != nil {
		return nil, wrapReleaseErr(errVerifyFailed, "stored release %s failed verification: %v", version, err)
	}

	prev := curState.Channels[channel]
	if channel == "stable" && prev != "" && prev != version {
		curState.PreviousStable = prev
	}
	curState.Channels[channel] = version

	if err := s.commitStateLocked(curState); err != nil {
		return nil, err
	}
	s.appendAudit(releaseAuditEntry{
		At:      utcNow(s.now()),
		Action:  "activate",
		Actor:   actor,
		Channel: channel,
		Version: version,
		Detail:  map[string]any{"previous": prev},
	})
	recCopy := *rec
	return &recCopy, nil
}

func (s *releaseStore) deleteVersion(version, actor string) error {
	if !isValidReleaseVersionString(version) {
		return wrapReleaseErr(errVersionNotFound, "invalid version format")
	}
	unlock, err := s.acquireExclusiveLock()
	if err != nil {
		return err
	}
	defer unlock()

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	if _, ok := curState.Versions[version]; !ok {
		return wrapReleaseErr(errVersionNotFound, "version %s is not stored", version)
	}
	for _, ch := range []string{"stable", "beta"} {
		if curState.Channels[ch] == version {
			return wrapReleaseErr(errVersionActive, "version %s is currently active on channel %s", version, ch)
		}
	}
	delete(curState.Versions, version)
	if curState.PreviousStable == version {
		curState.PreviousStable = ""
	}
	if err := s.commitStateLocked(curState); err != nil {
		return err
	}
	if err := safeRemoveAllNoSymlink(s.versionDir(version)); err != nil {
		fmt.Fprintf(os.Stderr, "release delete cleanup warning for %s: %v\n", version, err)
	}
	s.appendAudit(releaseAuditEntry{
		At:      utcNow(s.now()),
		Action:  "delete",
		Actor:   actor,
		Version: version,
	})
	return nil
}

func (s *releaseStore) runGC(actor string) ([]string, error) {
	unlock, err := s.acquireExclusiveLock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	evicted, err := s.commitAndGCLocked(curState)
	if err != nil {
		return nil, err
	}
	s.appendAudit(releaseAuditEntry{
		At:     utcNow(s.now()),
		Action: "gc",
		Actor:  actor,
		Detail: map[string]any{"evicted": evicted},
	})
	return evicted, nil
}

func (s *releaseStore) commitStateLocked(next *releaseState) error {
	snap, err := s.buildSnapshotLocked(next)
	if err != nil {
		return err
	}
	if err := s.saveStateLocked(next); err != nil {
		return err
	}
	s.stateMu.Lock()
	s.state = next
	s.stateMu.Unlock()
	s.snapshot.Store(snap)
	return nil
}

func (s *releaseStore) commitAndGCLocked(next *releaseState) ([]string, error) {
	protected := map[string]bool{}
	for _, ch := range []string{"stable", "beta"} {
		if v := next.Channels[ch]; v != "" {
			protected[v] = true
		}
	}
	if next.PreviousStable != "" {
		protected[next.PreviousStable] = true
	}

	var totalBytes int64
	candidates := make([]*releaseRecord, 0, len(next.Versions))
	for _, rec := range next.Versions {
		totalBytes += rec.ArchiveSize
		if !protected[rec.Version] {
			candidates = append(candidates, rec)
		}
	}
	// 由旧到新淘汰不在保护集合中的历史版本。
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].PublishedAt != candidates[j].PublishedAt {
			return candidates[i].PublishedAt < candidates[j].PublishedAt
		}
		return candidates[i].Version < candidates[j].Version
	})

	evicted := []string{}
	for _, rec := range candidates {
		if len(next.Versions) <= s.cfg.KeepCount && totalBytes <= s.cfg.MaxTotalBytes {
			break
		}
		delete(next.Versions, rec.Version)
		totalBytes -= rec.ArchiveSize
		evicted = append(evicted, rec.Version)
	}

	// 先原子提交索引与内存快照，再物理删除已摘除版本目录。
	if err := s.commitStateLocked(next); err != nil {
		return nil, err
	}
	for _, ver := range evicted {
		if err := safeRemoveAllNoSymlink(s.versionDir(ver)); err != nil {
			fmt.Fprintf(os.Stderr, "release gc remove %s warning: %v\n", ver, err)
		}
	}
	return evicted, nil
}

func (s *releaseStore) appendAudit(entry releaseAuditEntry) {
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if info, err := os.Stat(s.auditPath()); err == nil && info.Size() >= releaseAuditMaxBytes {
		_ = os.Rename(s.auditPath(), s.auditPath()+".1")
	}
	f, err := os.OpenFile(s.auditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(line)
	_ = f.Close()
}

func requiredDiskBytesForArchive(archiveSize uint64, minFreeBytes uint64) uint64 {
	const bufferBytes = uint64(256 << 20) // 256 MiB
	need := archiveSize*2 + bufferBytes
	if need < archiveSize { // 溢出保护
		return ^uint64(0)
	}
	if minFreeBytes > need {
		return minFreeBytes
	}
	return need
}

func cloneReleaseState(src *releaseState) *releaseState {
	dst := &releaseState{
		SchemaVersion:  src.SchemaVersion,
		Channels:       make(map[string]string, len(src.Channels)),
		PreviousStable: src.PreviousStable,
		Versions:       make(map[string]*releaseRecord, len(src.Versions)),
	}
	for k, v := range src.Channels {
		dst.Channels[k] = v
	}
	for k, v := range src.Versions {
		c := *v
		dst.Versions[k] = &c
	}
	return dst
}

func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func cleanDirContentsNoSymlink(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		if err := safeRemoveAllNoSymlink(full); err != nil {
			return err
		}
	}
	return nil
}

func safeRemoveAllNoSymlink(target string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove symlink %s", target)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := safeRemoveAllNoSymlink(filepath.Join(target, entry.Name())); err != nil {
				return err
			}
		}
		return os.Remove(target)
	}
	return os.Remove(target)
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
