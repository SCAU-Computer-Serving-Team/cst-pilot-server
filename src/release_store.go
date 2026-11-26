// 发行包分发 · 原子存储、生命周期级独占锁、崩溃自愈、历史版本配额回收与审计日志。
//
// 发行包目录与遥测 SQLite 物理隔离，热路径通过不可变内存快照无锁响应。
package main

import (
	"context"
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
	"syscall"
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
	errStoreOwnerLocked    = errors.New("storage_locked_by_another_process")
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

type releaseHistoryEntry struct {
	ArchiveSHA256    string `json:"archiveSha256"`
	ManifestSHA256   string `json:"manifestSha256"`
	FirstPublishedAt string `json:"firstPublishedAt"`
}

type releaseState struct {
	SchemaVersion  int                            `json:"schemaVersion"`
	Channels       map[string]string              `json:"channels"`
	PreviousStable string                         `json:"previousStable,omitempty"`
	Versions       map[string]*releaseRecord      `json:"versions"`
	History        map[string]releaseHistoryEntry `json:"history,omitempty"`
}

type releaseRecord struct {
	Version              string `json:"version"`
	Filename             string `json:"filename"`
	ArchiveSize          int64  `json:"archiveSize"`
	ManifestSize         int64  `json:"manifestSize"`
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

	// 生命周期级文件锁，确保同一存储目录同时只有一个所有者进程。
	ownerLockFile *os.File
	ownerLockMu   sync.Mutex

	writeBusy atomic.Bool
	stateMu   sync.Mutex
	state     *releaseState
	snapshot  atomic.Pointer[releaseSnapshot]

	// 测试故障注入钩子（正常为 nil）。
	beforeStateRename func() error
	afterStateRename  func() error
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
		_ = rs.close()
		return nil, err
	}
	return rs, nil
}

func (s *releaseStore) close() error {
	s.ownerLockMu.Lock()
	defer s.ownerLockMu.Unlock()
	if s.ownerLockFile == nil {
		return nil
	}
	_ = unlockFile(s.ownerLockFile)
	err := s.ownerLockFile.Close()
	s.ownerLockFile = nil
	return err
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
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("path %s must be a real directory", path)
		}
		return os.Chmod(path, perm)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
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

func rejectIfSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink rejected at %s", path)
	}
	return nil
}

func (s *releaseStore) acquireOwnerLock() error {
	if err := rejectIfSymlink(s.lockPath()); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	locked, err := tryLockFile(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	if !locked {
		_ = f.Close()
		return wrapReleaseErr(errStoreOwnerLocked, "release storage %s is held by another running process; stop it or omit --offline", s.cfg.StorageDir)
	}
	s.ownerLockMu.Lock()
	s.ownerLockFile = f
	s.ownerLockMu.Unlock()
	return nil
}

// tryAcquireWriteSlot 获取进程内独占写槽，确保并发上传/切版/删除/GC 串行且提前拒绝。
func (s *releaseStore) tryAcquireWriteSlot() (func(), error) {
	if !s.writeBusy.CompareAndSwap(false, true) {
		return nil, wrapReleaseErr(errReleaseBusy, "another release mutation is in progress")
	}
	return func() { s.writeBusy.Store(false) }, nil
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
	if err := s.acquireOwnerLock(); err != nil {
		return err
	}

	if err := cleanDirContentsNoSymlink(s.tmpDir()); err != nil {
		return fmt.Errorf("cleanup release tmp dir: %w", err)
	}

	state, existed, err := s.loadStateFile()
	if err != nil {
		return err
	}

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

	activeVersions := map[string]bool{}
	for _, ch := range []string{"stable", "beta"} {
		if v := state.Channels[ch]; v != "" {
			activeVersions[v] = true
		}
	}
	verifiedManifests := make(map[string][]byte, len(state.Versions))
	stateChanged := !existed
	for ver, rec := range state.Versions {
		if !isValidReleaseVersionString(ver) || rec == nil || rec.Version != ver {
			return fmt.Errorf("corrupt state entry for version %q", ver)
		}
		envBytes, manifestHash, err := s.verifyStoredVersionOnDisk(rec)
		if err != nil {
			if activeVersions[ver] {
				return fmt.Errorf("active release %s verification failed: %w", ver, err)
			}
			fmt.Fprintf(os.Stderr, "release recovery: inactive version %s invalid: %v\n", ver, err)
			delete(state.Versions, ver)
			if state.PreviousStable == ver {
				state.PreviousStable = ""
			}
			_ = safeRemoveAllNoSymlink(s.versionDir(ver))
			stateChanged = true
			continue
		}
		if rec.ManifestSize == 0 || rec.ManifestSHA256 != manifestHash {
			rec.ManifestSize = int64(len(envBytes))
			rec.ManifestSHA256 = manifestHash
			stateChanged = true
		}
		if _, ok := state.History[ver]; !ok {
			state.History[ver] = releaseHistoryEntry{
				ArchiveSHA256:    rec.ArchiveSHA256,
				ManifestSHA256:   rec.ManifestSHA256,
				FirstPublishedAt: rec.PublishedAt,
			}
			stateChanged = true
		}
		verifiedManifests[ver] = envBytes
	}
	for _, ch := range []string{"stable", "beta"} {
		if v := state.Channels[ch]; v != "" {
			if _, ok := state.Versions[v]; !ok {
				return fmt.Errorf("active channel %s points to missing version %s", ch, v)
			}
		}
	}

	snap, err := s.buildSnapshotFromVerified(state, verifiedManifests)
	if err != nil {
		return err
	}
	if stateChanged {
		if err := s.saveStateAtomic(state); err != nil {
			return err
		}
	}
	s.stateMu.Lock()
	s.state = state
	s.stateMu.Unlock()
	s.snapshot.Store(snap)
	return nil
}

func (s *releaseStore) loadStateFile() (*releaseState, bool, error) {
	if err := rejectIfSymlink(s.statePath()); err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return &releaseState{
			SchemaVersion: 1,
			Channels:      map[string]string{"stable": "", "beta": ""},
			Versions:      map[string]*releaseRecord{},
			History:       map[string]releaseHistoryEntry{},
		}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read release state: %w", err)
	}
	var st releaseState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, false, fmt.Errorf("parse release state: %w", err)
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
	if st.History == nil {
		st.History = map[string]releaseHistoryEntry{}
	}
	return &st, true, nil
}

// saveStateAtomic 原子替换 state.json；返回 (renamed, err)。
// 一旦 renamed == true，磁盘状态已提交为 st，调用方必须同步内存状态不可回退。
func (s *releaseStore) saveStateAtomicWithFlag(st *releaseState) (bool, error) {
	if err := rejectIfSymlink(s.statePath()); err != nil {
		return false, err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	tmpName := filepath.Join(s.tmpDir(), "state-"+randomHex(8)+".json")
	f, err := os.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false, err
	}
	if s.beforeStateRename != nil {
		if err := s.beforeStateRename(); err != nil {
			_ = os.Remove(tmpName)
			return false, err
		}
	}
	if err := os.Rename(tmpName, s.statePath()); err != nil {
		_ = os.Remove(tmpName)
		return false, err
	}
	if s.afterStateRename != nil {
		if err := s.afterStateRename(); err != nil {
			return true, err
		}
	}
	if err := syncDir(s.cfg.StorageDir); err != nil {
		return true, err
	}
	return true, nil
}

func (s *releaseStore) saveStateAtomic(st *releaseState) error {
	_, err := s.saveStateAtomicWithFlag(st)
	return err
}

func (s *releaseStore) verifyStoredVersionOnDisk(rec *releaseRecord) ([]byte, string, error) {
	vDir := s.versionDir(rec.Version)
	info, err := os.Lstat(vDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("version directory invalid")
	}
	manifestPath := filepath.Join(vDir, "manifest.json")
	mInfo, err := os.Lstat(manifestPath)
	if err != nil || !mInfo.Mode().IsRegular() || mInfo.Size() > maxManifestBytes {
		return nil, "", fmt.Errorf("manifest file invalid")
	}
	envBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", err
	}
	verified, err := verifyReleaseManifestEnvelope(envBytes, s.cfg.PublicKey, s.cfg.PublicBaseURL, s.cfg.MaxArchiveBytes)
	if err != nil {
		return nil, "", err
	}
	if verified.Info.Version != rec.Version || verified.Filename != rec.Filename {
		return nil, "", fmt.Errorf("manifest version or filename mismatch")
	}
	archivePath := filepath.Join(vDir, rec.Filename)
	aInfo, err := os.Lstat(archivePath)
	if err != nil || !aInfo.Mode().IsRegular() {
		return nil, "", fmt.Errorf("archive file missing or irregular")
	}
	if aInfo.Size() != rec.ArchiveSize || uint64(aInfo.Size()) != verified.Info.Archive.Size {
		return nil, "", fmt.Errorf("archive file size mismatch on disk")
	}
	return verified.Envelope, verified.ManifestSHA256, nil
}

func (s *releaseStore) buildSnapshotFromVerified(st *releaseState, verifiedManifests map[string][]byte) (*releaseSnapshot, error) {
	snap := &releaseSnapshot{
		channels:       make(map[string]*channelSnapshot, 2),
		files:          make(map[string]*fileSnapshot, len(st.Versions)),
		previousStable: st.PreviousStable,
		records:        make([]*releaseRecord, 0, len(st.Versions)),
	}
	for ver, rec := range st.Versions {
		recCopy := *rec
		snap.records = append(snap.records, &recCopy)
		snap.totalBytes += rec.ArchiveSize + rec.ManifestSize
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
		var envBytes []byte
		var manifestHash string
		if cached, ok := verifiedManifests[ver]; ok {
			envBytes = cached
			sum := sha256.Sum256(envBytes)
			manifestHash = hex.EncodeToString(sum[:])
		} else {
			var err error
			envBytes, manifestHash, err = s.verifyStoredVersionOnDisk(rec)
			if err != nil {
				return nil, fmt.Errorf("verify active manifest %s/%s: %w", ch, ver, err)
			}
		}
		snap.channels[ch] = &channelSnapshot{
			Version:        ver,
			ManifestBytes:  envBytes,
			ManifestSHA256: manifestHash,
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

// publishStreaming 获取独占写槽并执行原子发布。
func (s *releaseStore) publishStreaming(
	ctx context.Context,
	channel string,
	activate bool,
	manifestBytes []byte,
	nextArchive func() (io.Reader, error),
	afterArchive func() error,
	actor string,
) (*publishResult, error) {
	releaseSlot, err := s.tryAcquireWriteSlot()
	if err != nil {
		return nil, err
	}
	defer releaseSlot()
	return s.publishStreamingLocked(ctx, channel, activate, manifestBytes, nextArchive, afterArchive, actor)
}

// publishStreamingLocked 供已经提前持有独占写槽的调用方（如 HTTP 上传入口）使用。
func (s *releaseStore) publishStreamingLocked(
	ctx context.Context,
	channel string,
	activate bool,
	manifestBytes []byte,
	nextArchive func() (io.Reader, error),
	afterArchive func() error,
	actor string,
) (*publishResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !isValidReleaseChannel(channel) {
		return nil, wrapReleaseErr(errInvalidChannel, "channel must be stable or beta")
	}

	verified, err := verifyReleaseManifestEnvelope(manifestBytes, s.cfg.PublicKey, s.cfg.PublicBaseURL, s.cfg.MaxArchiveBytes)
	if err != nil {
		if errors.Is(err, errArchiveTooLarge) {
			return nil, err
		}
		return nil, wrapReleaseErr(errVerifyFailed, "%v", err)
	}
	ver := verified.Info.Version

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	// 永久禁止复用已发布过（含已 GC/已删除）的版本号，保护 immutable 归档缓存一致。
	if _, exists := curState.Versions[ver]; exists {
		return nil, wrapReleaseErr(errVersionExists, "version %s already exists and is immutable", ver)
	}
	if _, seenBefore := curState.History[ver]; seenBefore {
		return nil, wrapReleaseErr(errVersionExists, "version %s was previously published and cannot be reused", ver)
	}
	if _, err := os.Lstat(s.versionDir(ver)); err == nil {
		return nil, wrapReleaseErr(errVersionExists, "version directory %s already exists", ver)
	}

	// 磁盘空间水位预检（在接收压缩包前拦截）。
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
		return nil, mapStorageIOError("create temp archive file", err)
	}

	limit := int64(verified.Info.Archive.Size)
	if s.cfg.MaxArchiveBytes < limit {
		limit = s.cfg.MaxArchiveBytes
	}
	hasher := sha256.New()
	bufPtr := releaseBufPool.Get().(*[]byte)
	written, copyErr := copyStreamWithContext(ctx, io.MultiWriter(archiveFile, hasher), io.LimitReader(archiveReader, limit+1), *bufPtr)
	releaseBufPool.Put(bufPtr)
	if copyErr != nil {
		_ = archiveFile.Close()
		return nil, mapStorageIOError("stream archive upload", copyErr)
	}
	if written > limit {
		_ = archiveFile.Close()
		return nil, wrapReleaseErr(errArchiveTooLarge, "archive exceeds declared or configured size limit")
	}
	if afterArchive != nil {
		if err := afterArchive(); err != nil {
			_ = archiveFile.Close()
			return nil, err
		}
	}
	if uint64(written) != verified.Info.Archive.Size {
		_ = archiveFile.Close()
		return nil, wrapReleaseErr(errVerifyFailed, "archive size mismatch: uploaded %d want %d", written, verified.Info.Archive.Size)
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA256 != verified.Info.Archive.SHA256 {
		_ = archiveFile.Close()
		return nil, wrapReleaseErr(errVerifyFailed, "archive sha256 mismatch")
	}
	if err := archiveFile.Sync(); err != nil {
		_ = archiveFile.Close()
		return nil, mapStorageIOError("fsync archive", err)
	}

	// 零落盘流式校验 ZIP 中央目录与全树内容（检查上下文取消）。
	if err := verifyReleaseArchiveStreaming(ctx, archiveFile, written, verified); err != nil {
		_ = archiveFile.Close()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, wrapReleaseErr(errVerifyFailed, "%v", err)
	}
	if err := archiveFile.Close(); err != nil {
		return nil, mapStorageIOError("close archive", err)
	}

	manifestStagePath := filepath.Join(stageDir, "manifest.json")
	if err := writeFileSync(manifestStagePath, verified.Envelope, 0o644); err != nil {
		return nil, mapStorageIOError("write manifest file", err)
	}
	if err := os.Chmod(archiveStagePath, 0o644); err != nil {
		return nil, fmt.Errorf("chmod archive: %w", err)
	}
	if err := os.Chmod(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("chmod version stage dir: %w", err)
	}
	if err := syncDir(stageDir); err != nil {
		return nil, mapStorageIOError("fsync stage dir", err)
	}

	// 在进入不可逆的目录移动与状态提交之前，最后检查一次请求取消。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	targetDir := s.versionDir(ver)
	if err := os.Rename(stageDir, targetDir); err != nil {
		return nil, mapStorageIOError("atomic rename version dir", err)
	}
	cleanupStage = false
	rollbackTarget := true
	defer func() {
		if rollbackTarget {
			_ = safeRemoveAllNoSymlink(targetDir)
		}
	}()

	if err := syncDir(s.versionsDir()); err != nil {
		return nil, mapStorageIOError("fsync versions dir", err)
	}

	nowStr := utcNow(s.now())
	rec := &releaseRecord{
		Version:              ver,
		Filename:             verified.Filename,
		ArchiveSize:          written,
		ManifestSize:         int64(len(verified.Envelope)),
		UnpackedSize:         verified.Info.Archive.UnpackedSize,
		ArchiveSHA256:        actualSHA256,
		ManifestSHA256:       verified.ManifestSHA256,
		BuiltAt:              verified.Info.BuiltAt,
		PublishedAt:          nowStr,
		MinUpgradableVersion: verified.Info.MinUpgradableVersion,
	}
	curState.Versions[ver] = rec
	curState.History[ver] = releaseHistoryEntry{
		ArchiveSHA256:    actualSHA256,
		ManifestSHA256:   verified.ManifestSHA256,
		FirstPublishedAt: nowStr,
	}
	if activate {
		if channel == "stable" && curState.Channels["stable"] != "" && curState.Channels["stable"] != ver {
			curState.PreviousStable = curState.Channels["stable"]
		}
		curState.Channels[channel] = ver
	}

	// 新发布版本自身进入本次 GC 保护集合，避免 --no-activate 刚入库即被当场淘汰。
	evicted, stateCommitted, err := s.commitAndGCLocked(curState, map[string][]byte{ver: verified.Envelope}, ver)
	if stateCommitted {
		rollbackTarget = false
	}
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
	releaseSlot, err := s.tryAcquireWriteSlot()
	if err != nil {
		return nil, err
	}
	defer releaseSlot()

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	rec, ok := curState.Versions[version]
	if !ok {
		return nil, wrapReleaseErr(errVersionNotFound, "version %s is not stored", version)
	}
	envBytes, _, err := s.verifyStoredVersionOnDisk(rec)
	if err != nil {
		return nil, wrapReleaseErr(errVerifyFailed, "stored release %s failed verification: %v", version, err)
	}

	prev := curState.Channels[channel]
	if channel == "stable" && prev != "" && prev != version {
		curState.PreviousStable = prev
	}
	curState.Channels[channel] = version

	if _, err := s.commitStateLocked(curState, map[string][]byte{version: envBytes}); err != nil {
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
	releaseSlot, err := s.tryAcquireWriteSlot()
	if err != nil {
		return err
	}
	defer releaseSlot()

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
	if _, err := s.commitStateLocked(curState, nil); err != nil {
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
	releaseSlot, err := s.tryAcquireWriteSlot()
	if err != nil {
		return nil, err
	}
	defer releaseSlot()

	s.stateMu.Lock()
	curState := cloneReleaseState(s.state)
	s.stateMu.Unlock()

	evicted, _, err := s.commitAndGCLocked(curState, nil, "")
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

// commitStateLocked 严格区分 Rename(state) 前后边界：
// Rename 成功即立刻同步内存状态与快照，绝不出现磁盘已变而内存仍用旧状态的分裂。
func (s *releaseStore) commitStateLocked(next *releaseState, verifiedManifests map[string][]byte) (bool, error) {
	snap, err := s.buildSnapshotFromVerified(next, verifiedManifests)
	if err != nil {
		return false, err
	}
	renamed, saveErr := s.saveStateAtomicWithFlag(next)
	if renamed {
		s.stateMu.Lock()
		s.state = next
		s.stateMu.Unlock()
		s.snapshot.Store(snap)
	}
	return renamed, saveErr
}

func (s *releaseStore) commitAndGCLocked(next *releaseState, verifiedManifests map[string][]byte, extraProtected string) ([]string, bool, error) {
	protected := map[string]bool{}
	for _, ch := range []string{"stable", "beta"} {
		if v := next.Channels[ch]; v != "" {
			protected[v] = true
		}
	}
	if next.PreviousStable != "" {
		protected[next.PreviousStable] = true
	}
	if extraProtected != "" {
		protected[extraProtected] = true
	}

	var totalBytes int64
	candidates := make([]*releaseRecord, 0, len(next.Versions))
	for _, rec := range next.Versions {
		totalBytes += rec.ArchiveSize + rec.ManifestSize
		if !protected[rec.Version] {
			candidates = append(candidates, rec)
		}
	}
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
		totalBytes -= rec.ArchiveSize + rec.ManifestSize
		evicted = append(evicted, rec.Version)
	}

	committed, err := s.commitStateLocked(next, verifiedManifests)
	if !committed {
		return nil, false, err
	}
	for _, ver := range evicted {
		if rmErr := safeRemoveAllNoSymlink(s.versionDir(ver)); rmErr != nil {
			fmt.Fprintf(os.Stderr, "release gc remove %s warning: %v\n", ver, rmErr)
		}
	}
	return evicted, true, err
}

func (s *releaseStore) appendAudit(entry releaseAuditEntry) {
	if err := rejectIfSymlink(s.auditPath()); err != nil {
		return
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if info, err := os.Lstat(s.auditPath()); err == nil && info.Mode().IsRegular() && info.Size() >= releaseAuditMaxBytes {
		_ = os.Rename(s.auditPath(), s.auditPath()+".1")
	}
	f, err := os.OpenFile(s.auditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(line)
	_ = f.Close()
}

func copyStreamWithContext(ctx context.Context, dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return written, nil
		}
		if er != nil {
			return written, er
		}
	}
}

func mapStorageIOError(action string, err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return wrapReleaseErr(errInsufficientStorage, "%s: no space left on device", action)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func requiredDiskBytesForArchive(archiveSize uint64, minFreeBytes uint64) uint64 {
	const bufferBytes = uint64(256 << 20) // 256 MiB
	need := archiveSize*2 + bufferBytes
	if need < archiveSize {
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
		History:        make(map[string]releaseHistoryEntry, len(src.History)),
	}
	for k, v := range src.Channels {
		dst.Channels[k] = v
	}
	for k, v := range src.Versions {
		c := *v
		dst.Versions[k] = &c
	}
	for k, v := range src.History {
		dst.History[k] = v
	}
	return dst
}

func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
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
