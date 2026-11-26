// 发行包分发 · 双重 Ed25519 严格验签与零落盘流式 ZIP 全树核验。
//
// 校验规则与主仓 crates/cst-updater/src/{manifest,verify,layout}.rs 保持一致。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxManifestBytes      = 8 * 1024 * 1024
	maxClientArchiveBytes = int64(2) << 30
	maxClientUnpacked     = uint64(8) << 30
	maxReleaseFiles       = 100_000
	maxReleaseNotesBytes  = 16 * 1024
	maxSha256SumsBytes    = 6 * 1024 * 1024
	maxCentralDirBytes    = uint64(64) << 20
	maxJSONDepth          = 64

	manifestDomain = "cst-pilot-release-manifest-v1\n"
	sumsDomain     = "cst-pilot-release-sums-v1\n"

	releaseSHA256SumsPath = "app/SHA256SUMS"
	releaseBuildInfoPath  = "app/BUILD-INFO.json"
	releaseServerExePath  = "app/cst-server.exe"
	releaseUpdaterExePath = "app/cst-updater.exe"
)

var (
	releaseVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	secretScanPattern     = regexp.MustCompile(`\b(?:sk-(?:proj-|ant-api[0-9]+-)?[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AIza[0-9A-Za-z_-]{30,}|AKIA[0-9A-Z]{16})|eyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)
)

var requiredReleaseBaseFiles = []string{
	"cst-pilot.exe",
	releaseServerExePath,
	releaseUpdaterExePath,
	"VERSION",
	releaseBuildInfoPath,
	"LICENSE",
	"THIRD-PARTY-NOTICES.md",
	"app/prompts/APPEND_SYSTEM.md",
	"app/telemetry/timserver_1.crt",
	"app/telemetry/currency.json",
}

var requiredReleaseDefaultConfigs = []string{
	"app/defaults/models-store.json",
	"app/defaults/telemetry.json",
	"app/defaults/web-search.json",
	"app/defaults/settings.json",
}

var requiredReleaseFontFiles = []string{
	"app/fonts/SourceHanSansCN-VF.otf",
	"app/fonts/LICENSE.txt",
	"app/fonts/JetBrainsMono-Regular.ttf",
	"app/fonts/JetBrainsMono-Medium.ttf",
	"app/fonts/JetBrainsMono-OFL.txt",
}

var requiredReleaseSFNTFonts = map[string][4]byte{
	"app/fonts/SourceHanSansCN-VF.otf":    {'O', 'T', 'T', 'O'},
	"app/fonts/JetBrainsMono-Regular.ttf": {0, 1, 0, 0},
	"app/fonts/JetBrainsMono-Medium.ttf":  {0, 1, 0, 0},
}

var requiredReleaseSkills = []string{
	"disk",
	"ls",
	"files",
	"sys",
	"startup",
	"eventlog",
	"driver",
	"runbook",
	"bitlocker",
	"web-search",
}

var mergeReleaseFiles = map[string]bool{
	"data/config/models-store.json": true,
	"data/config/telemetry.json":    true,
	"data/config/web-search.json":   true,
	"data/config/settings.json":     true,
}

var forbiddenJSONSecretKeys = map[string]bool{
	"apikey":        true,
	"accesstoken":   true,
	"refreshtoken":  true,
	"token":         true,
	"secret":        true,
	"password":      true,
	"clientsecret":  true,
	"authorization": true,
	"identitykey":   true,
}

var releaseBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

type signedManifestEnvelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type releaseManifest struct {
	SchemaVersion        uint32             `json:"schemaVersion"`
	Platform             string             `json:"platform"`
	Version              string             `json:"version"`
	BuiltAt              string             `json:"builtAt"`
	MinUpgradableVersion string             `json:"minUpgradableVersion"`
	ReleaseNotes         string             `json:"releaseNotes"`
	Archive              releaseArchiveMeta `json:"archive"`
	SHA256Sums           string             `json:"sha256Sums"`
	SHA256Signature      string             `json:"sha256Signature"`
}

type releaseArchiveMeta struct {
	URL          string `json:"url"`
	Size         uint64 `json:"size"`
	UnpackedSize uint64 `json:"unpackedSize"`
	SHA256       string `json:"sha256"`
}

type verifiedReleaseManifest struct {
	Info           releaseManifest
	Sums           map[string]string
	Envelope       []byte
	ManifestSHA256 string
	Filename       string
}

func expectedReleaseArchiveFilename(version string) string {
	return "cst-pilot-" + version + "-windows-x64.zip"
}

func expectedReleaseArchiveURL(baseURL, version string) string {
	return strings.TrimRight(baseURL, "/") + "/files/" + version + "/" + expectedReleaseArchiveFilename(version)
}

func isValidReleaseChannel(ch string) bool {
	return ch == "stable" || ch == "beta"
}

func isValidReleaseVersionString(v string) bool {
	if len(v) == 0 || len(v) > 64 || strings.TrimSpace(v) != v {
		return false
	}
	if !releaseVersionPattern.MatchString(v) {
		return false
	}
	_, err := parseSemver(v)
	return err == nil
}

// verifyReleaseManifestEnvelope 执行与客户端等价的双重 Ed25519 严格验签与字段校验。
func verifyReleaseManifestEnvelope(envelope []byte, pubKey ed25519.PublicKey, publicBaseURL string, maxArchiveBytes int64) (*verifiedReleaseManifest, error) {
	if len(envelope) == 0 || len(envelope) > maxManifestBytes {
		return nil, fmt.Errorf("manifest exceeds size limits")
	}
	if err := validateStrictJSON(envelope); err != nil {
		return nil, fmt.Errorf("manifest JSON invalid: %w", err)
	}
	var signed signedManifestEnvelope
	if err := decodeStrictJSONStruct(envelope, &signed); err != nil {
		return nil, fmt.Errorf("signed manifest format invalid: %w", err)
	}
	if signed.Payload == "" || signed.Signature == "" {
		return nil, fmt.Errorf("signed manifest missing payload or signature")
	}
	if err := verifyStrictEd25519Domain(pubKey, manifestDomain, []byte(signed.Payload), signed.Signature); err != nil {
		return nil, fmt.Errorf("manifest signature verification failed: %w", err)
	}
	if err := validateStrictJSON([]byte(signed.Payload)); err != nil {
		return nil, fmt.Errorf("manifest payload JSON invalid: %w", err)
	}
	var info releaseManifest
	if err := decodeStrictJSONStruct([]byte(signed.Payload), &info); err != nil {
		return nil, fmt.Errorf("manifest payload fields invalid: %w", err)
	}
	if info.SchemaVersion != 1 || info.Platform != "windows-x86_64" {
		return nil, fmt.Errorf("unsupported schemaVersion or platform")
	}
	if len(info.ReleaseNotes) > maxReleaseNotesBytes || len(info.SHA256Sums) > maxSha256SumsBytes {
		return nil, fmt.Errorf("releaseNotes or sha256Sums exceeds size limit")
	}
	if info.Archive.Size == 0 || info.Archive.Size > uint64(maxClientArchiveBytes) || (maxArchiveBytes > 0 && info.Archive.Size > uint64(maxArchiveBytes)) {
		return nil, fmt.Errorf("archive size exceeds allowed limit")
	}
	if info.Archive.UnpackedSize == 0 || info.Archive.UnpackedSize > maxClientUnpacked {
		return nil, fmt.Errorf("archive unpackedSize out of range")
	}
	if !isLowerHexSHA256(info.Archive.SHA256) {
		return nil, fmt.Errorf("invalid archive sha256")
	}
	if !isValidReleaseVersionString(info.Version) {
		return nil, fmt.Errorf("invalid release version format")
	}
	ver, err := parseSemver(info.Version)
	if err != nil {
		return nil, fmt.Errorf("invalid release version: %w", err)
	}
	minVer, err := parseSemver(info.MinUpgradableVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid minUpgradableVersion: %w", err)
	}
	if compareSemver(minVer, ver) > 0 {
		return nil, fmt.Errorf("minUpgradableVersion exceeds version")
	}
	if _, err := time.Parse(time.RFC3339, info.BuiltAt); err != nil {
		return nil, fmt.Errorf("invalid builtAt timestamp")
	}
	if _, err := validateReleaseHTTPSURL(info.Archive.URL); err != nil {
		return nil, fmt.Errorf("invalid archive url: %w", err)
	}
	expectedURL := expectedReleaseArchiveURL(publicBaseURL, info.Version)
	if publicBaseURL != "" && info.Archive.URL != expectedURL {
		return nil, fmt.Errorf("archive url must match canonical endpoint %s", expectedURL)
	}
	if err := verifyStrictEd25519Domain(pubKey, sumsDomain, []byte(info.SHA256Sums), info.SHA256Signature); err != nil {
		return nil, fmt.Errorf("sha256Sums signature verification failed: %w", err)
	}
	sums, err := parseReleaseSums(info.SHA256Sums)
	if err != nil {
		return nil, err
	}
	if err := checkRequiredManifestPaths(sums); err != nil {
		return nil, err
	}
	envCopy := append([]byte(nil), envelope...)
	digest := sha256.Sum256(envCopy)
	return &verifiedReleaseManifest{
		Info:           info,
		Sums:           sums,
		Envelope:       envCopy,
		ManifestSHA256: hex.EncodeToString(digest[:]),
		Filename:       expectedReleaseArchiveFilename(info.Version),
	}, nil
}

func checkRequiredManifestPaths(sums map[string]string) error {
	for _, p := range requiredReleaseBaseFiles {
		if _, ok := sums[p]; !ok {
			return fmt.Errorf("missing required file in sha256Sums: %s", p)
		}
	}
	for _, p := range requiredReleaseDefaultConfigs {
		if _, ok := sums[p]; !ok {
			return fmt.Errorf("missing required default config in sha256Sums: %s", p)
		}
	}
	for _, p := range requiredReleaseFontFiles {
		if _, ok := sums[p]; !ok {
			return fmt.Errorf("missing required font file in sha256Sums: %s", p)
		}
	}
	for _, skill := range requiredReleaseSkills {
		p := "app/skills/" + skill + "/SKILL.md"
		if _, ok := sums[p]; !ok {
			return fmt.Errorf("missing required skill in sha256Sums: %s", p)
		}
	}
	hasLicense := false
	for p := range sums {
		if strings.HasPrefix(p, "app/licenses/") {
			hasLicense = true
			break
		}
	}
	if !hasLicense {
		return fmt.Errorf("missing third-party license files under app/licenses/")
	}
	return nil
}

// verifyReleaseArchiveStreaming 直接基于临时磁盘文件做流式 ZIP 全树校验，不解压落盘。
func verifyReleaseArchiveStreaming(file *os.File, archiveSize int64, manifest *verifiedReleaseManifest) error {
	if uint64(archiveSize) != manifest.Info.Archive.Size {
		return fmt.Errorf("archive size mismatch")
	}
	if err := validateZipBounds(file, archiveSize); err != nil {
		return err
	}
	zr, err := zip.NewReader(file, archiveSize)
	if err != nil {
		return fmt.Errorf("invalid zip archive: %w", err)
	}
	if len(zr.File) == 0 || len(zr.File) > maxReleaseFiles*2 {
		return fmt.Errorf("zip entry count out of range")
	}

	dirPrefixes := make(map[string]bool)
	for p := range manifest.Sums {
		parts := strings.Split(p, "/")
		prefix := ""
		for i := 0; i < len(parts)-1; i++ {
			if prefix != "" {
				prefix += "/"
			}
			prefix += parts[i]
			dirPrefixes[prefix] = true
		}
	}

	seenLower := make(map[string]bool, len(zr.File))
	spellings := make(map[string]string, len(zr.File)*2)
	seenFiles := make(map[string]bool, len(manifest.Sums)+1)
	var totalUnpacked uint64
	var versionContent []byte
	var buildInfoContent []byte

	bufPtr := releaseBufPool.Get().(*[]byte)
	defer releaseBufPool.Put(bufPtr)
	buf := *bufPtr

	for _, f := range zr.File {
		if f.NonUTF8 || !utf8.ValidString(f.Name) {
			return fmt.Errorf("zip entry name must be valid UTF-8")
		}
		isDir := f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/")
		raw := f.Name
		if isDir {
			raw = strings.TrimSuffix(raw, "/")
		}
		if raw == "cst-pilot" && isDir {
			continue
		}
		if !strings.HasPrefix(raw, "cst-pilot/") {
			return fmt.Errorf("zip entry outside cst-pilot/ top directory: %s", raw)
		}
		rel := strings.TrimPrefix(raw, "cst-pilot/")
		if err := validateSafeReleasePath(rel); err != nil {
			return err
		}
		lowerRel := strings.ToLower(rel)
		if seenLower[lowerRel] {
			return fmt.Errorf("duplicate or case-conflicting zip entry: %s", rel)
		}
		seenLower[lowerRel] = true
		if err := checkPathSpelling(rel, spellings); err != nil {
			return err
		}
		if err := validateZipEntryMetadata(f, isDir); err != nil {
			return err
		}
		if isDir {
			if err := rejectRetiredReleasePath(rel); err != nil {
				return err
			}
			if f.UncompressedSize64 != 0 || !dirPrefixes[rel] {
				return fmt.Errorf("zip directory not allowed by signed sums: %s", rel)
			}
			continue
		}
		if err := classifyReleaseProgramPath(rel); err != nil {
			return err
		}
		expectedHash, inSums := manifest.Sums[rel]
		isSumsFile := rel == releaseSHA256SumsPath
		if !inSums && !isSumsFile {
			return fmt.Errorf("zip contains unlisted file: %s", rel)
		}
		entryLimit := manifest.Info.Archive.UnpackedSize
		if isSumsFile {
			entryLimit = uint64(len(manifest.Info.SHA256Sums))
		}
		if f.UncompressedSize64 > entryLimit {
			return fmt.Errorf("zip file exceeds size limit: %s", rel)
		}
		if totalUnpacked > manifest.Info.Archive.UnpackedSize-f.UncompressedSize64 {
			return fmt.Errorf("zip unpacked size exceeds signed declaration")
		}

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("cannot open zip entry %s: %w", rel, err)
		}
		readBytes, actualHash, headBuf, jsonBytes, scanErr := streamVerifyZipEntry(rc, rel, entryLimit, buf)
		closeErr := rc.Close()
		if scanErr != nil {
			return scanErr
		}
		if closeErr != nil {
			return fmt.Errorf("zip entry checksum failed for %s: %w", rel, closeErr)
		}
		if readBytes != f.UncompressedSize64 {
			return fmt.Errorf("zip entry declared size mismatch: %s", rel)
		}
		totalUnpacked += readBytes

		if isSumsFile {
			if !bytes.Equal(jsonBytes, []byte(manifest.Info.SHA256Sums)) {
				return fmt.Errorf("archive app/SHA256SUMS does not match signed manifest")
			}
		} else {
			if actualHash != expectedHash {
				return fmt.Errorf("archive file sha256 mismatch: %s", rel)
			}
			if magic, ok := requiredReleaseSFNTFonts[rel]; ok {
				if err := validateSFNTHeader(headBuf, readBytes, magic); err != nil {
					return fmt.Errorf("invalid font %s: %w", rel, err)
				}
			}
			if rel == "VERSION" {
				versionContent = jsonBytes
			} else if rel == releaseBuildInfoPath {
				buildInfoContent = jsonBytes
			}
		}
		seenFiles[rel] = true
	}

	if totalUnpacked != manifest.Info.Archive.UnpackedSize {
		return fmt.Errorf("archive unpackedSize mismatch: got %d want %d", totalUnpacked, manifest.Info.Archive.UnpackedSize)
	}
	if len(seenFiles) != len(manifest.Sums)+1 || !seenFiles[releaseSHA256SumsPath] {
		return fmt.Errorf("archive missing signed manifest files")
	}
	for p := range manifest.Sums {
		if !seenFiles[p] {
			return fmt.Errorf("archive missing file: %s", p)
		}
	}
	if err := validateArchiveMetadataFiles(versionContent, buildInfoContent, manifest.Info.Version); err != nil {
		return err
	}
	return nil
}

func streamVerifyZipEntry(rc io.Reader, rel string, limit uint64, buf []byte) (uint64, string, []byte, []byte, error) {
	isSums := rel == releaseSHA256SumsPath
	isJSON := strings.HasSuffix(rel, ".json")
	isVersion := rel == "VERSION"
	_, isFont := requiredReleaseSFNTFonts[rel]

	var capture []byte
	var captureLimit uint64
	switch {
	case isSums:
		captureLimit = limit
	case isVersion:
		captureLimit = 128
	case rel == releaseBuildInfoPath:
		captureLimit = 64 * 1024
	case isJSON:
		captureLimit = 8 * 1024 * 1024
	}

	var fontHead []byte
	const maxFontHeaderBytes = 12 + 256*16

	hasher := sha256.New()
	var readTotal uint64
	var secretTail []byte

	for {
		n, err := rc.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if readTotal > limit || uint64(n) > limit-readTotal {
				return 0, "", nil, nil, fmt.Errorf("zip entry exceeds limit: %s", rel)
			}
			readTotal += uint64(n)
			hasher.Write(chunk)

			if !isSums {
				secretTail = append(secretTail, chunk...)
				if secretScanPattern.Match(secretTail) {
					return 0, "", nil, nil, fmt.Errorf("archive file contains credential pattern: %s", rel)
				}
				if len(secretTail) > 512 {
					secretTail = append(secretTail[:0], secretTail[len(secretTail)-512:]...)
				}
			}
			if captureLimit > 0 {
				if uint64(len(capture))+uint64(n) > captureLimit {
					return 0, "", nil, nil, fmt.Errorf("metadata/json file exceeds size limit: %s", rel)
				}
				capture = append(capture, chunk...)
			}
			if isFont && len(fontHead) < maxFontHeaderBytes {
				need := maxFontHeaderBytes - len(fontHead)
				if need > n {
					need = n
				}
				fontHead = append(fontHead, chunk[:need]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, "", nil, nil, fmt.Errorf("zip entry read error %s: %w", rel, err)
		}
	}

	if isJSON {
		if err := validateStrictJSON(capture); err != nil {
			return 0, "", nil, nil, fmt.Errorf("archive json invalid %s: %w", rel, err)
		}
		var parsed any
		if err := json.Unmarshal(capture, &parsed); err != nil {
			return 0, "", nil, nil, fmt.Errorf("archive json parse failed %s: %w", rel, err)
		}
		if err := checkJSONNoSecrets(parsed); err != nil {
			return 0, "", nil, nil, fmt.Errorf("archive json %s: %w", rel, err)
		}
	}

	return readTotal, hex.EncodeToString(hasher.Sum(nil)), fontHead, capture, nil
}

func validateArchiveMetadataFiles(versionBytes, buildInfoBytes []byte, expectedVersion string) error {
	if !utf8.Valid(versionBytes) {
		return fmt.Errorf("VERSION is not valid UTF-8")
	}
	foundVer := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(versionBytes), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "cst-pilot ") {
			foundVer = strings.TrimPrefix(line, "cst-pilot ")
			break
		}
	}
	if foundVer == "" || foundVer != expectedVersion {
		return fmt.Errorf("VERSION does not match signed manifest version")
	}
	var buildInfo map[string]any
	if err := json.Unmarshal(buildInfoBytes, &buildInfo); err != nil {
		return fmt.Errorf("app/BUILD-INFO.json invalid")
	}
	if v, _ := buildInfo["version"].(string); v != expectedVersion {
		return fmt.Errorf("app/BUILD-INFO.json version mismatch")
	}
	return nil
}

func validateSFNTHeader(head []byte, totalLen uint64, expectedMagic [4]byte) error {
	if len(head) < 28 || totalLen < 28 {
		return fmt.Errorf("font header truncated")
	}
	if [4]byte(head[:4]) != expectedMagic {
		return fmt.Errorf("font magic mismatch")
	}
	numTables := int(binary.BigEndian.Uint16(head[4:6]))
	dirEnd := 12 + numTables*16
	if numTables == 0 || dirEnd > len(head) || uint64(dirEnd) > totalLen {
		return fmt.Errorf("font table directory invalid")
	}
	hasCmap, hasName := false, false
	for i := 0; i < numTables; i++ {
		off := 12 + i*16
		tag := string(head[off : off+4])
		tblOffset := uint64(binary.BigEndian.Uint32(head[off+8 : off+12]))
		tblLen := uint64(binary.BigEndian.Uint32(head[off+12 : off+16]))
		if tblOffset > totalLen || tblLen > totalLen-tblOffset {
			return fmt.Errorf("font table out of bounds")
		}
		if tag == "cmap" {
			hasCmap = true
		} else if tag == "name" {
			hasName = true
		}
	}
	if !hasCmap || !hasName {
		return fmt.Errorf("font missing cmap or name table")
	}
	return nil
}

func validateZipEntryMetadata(f *zip.File, isDir bool) error {
	if f.Flags&0x1 != 0 {
		return fmt.Errorf("encrypted zip entry rejected: %s", f.Name)
	}
	if f.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("symlink zip entry rejected: %s", f.Name)
	}
	unixMode := (f.ExternalAttrs >> 16) & 0o170000
	expectedMode := uint32(0o100000)
	if isDir {
		expectedMode = 0o040000
	}
	if unixMode != 0 && unixMode != expectedMode {
		return fmt.Errorf("special file mode rejected: %s", f.Name)
	}
	return checkZipExtraFields(f.Extra)
}

func checkZipExtraFields(extra []byte) error {
	for len(extra) > 0 {
		if len(extra) < 4 {
			return fmt.Errorf("malformed zip extra field")
		}
		id := binary.LittleEndian.Uint16(extra[0:2])
		sz := int(binary.LittleEndian.Uint16(extra[2:4]))
		switch id {
		case 0x0001, 0x5455, 0x7875, 0x000a:
		default:
			return fmt.Errorf("unsupported zip extra field id 0x%04x", id)
		}
		if sz > len(extra)-4 {
			return fmt.Errorf("truncated zip extra field")
		}
		extra = extra[4+sz:]
	}
	return nil
}

// validateZipBounds 对齐 crates/cst-updater/src/verify.rs::zip_bounds。
func validateZipBounds(file *os.File, fileLen int64) error {
	if fileLen < 22 {
		return fmt.Errorf("zip archive too small")
	}
	start := fileLen - 65557
	if start < 0 {
		start = 0
	}
	tailLen := int(fileLen - start)
	tail := make([]byte, tailLen)
	if _, err := file.ReadAt(tail, start); err != nil {
		return fmt.Errorf("cannot read zip tail: %w", err)
	}
	at := -1
	for i := tailLen - 22; i >= 0; i-- {
		if bytes.Equal(tail[i:i+4], []byte{'P', 'K', 0x05, 0x06}) {
			commentLen := int(binary.LittleEndian.Uint16(tail[i+20 : i+22]))
			if i+22+commentLen == tailLen {
				at = i
				break
			}
		}
	}
	if at < 0 {
		return fmt.Errorf("missing zip EOCD")
	}
	eocd := tail[at:]
	if !bytes.Equal(eocd[4:8], []byte{0, 0, 0, 0}) || !bytes.Equal(eocd[8:10], eocd[10:12]) {
		return fmt.Errorf("multi-disk zip rejected")
	}
	count := uint64(binary.LittleEndian.Uint16(eocd[10:12]))
	cdSize := uint64(binary.LittleEndian.Uint32(eocd[12:16]))
	cdOffset := uint64(binary.LittleEndian.Uint32(eocd[16:20]))
	eocdPos := uint64(start) + uint64(at)

	if count == 0xffff || cdSize == 0xffff_ffff || cdOffset == 0xffff_ffff {
		if at < 20 {
			return fmt.Errorf("missing zip64 locator")
		}
		locator := tail[at-20 : at]
		if !bytes.Equal(locator[:8], []byte{'P', 'K', 0x06, 0x07, 0, 0, 0, 0}) ||
			!bytes.Equal(locator[16:20], []byte{1, 0, 0, 0}) {
			return fmt.Errorf("invalid zip64 locator")
		}
		pos := binary.LittleEndian.Uint64(locator[8:16])
		if pos+56 < pos || pos+56 > eocdPos-20 {
			return fmt.Errorf("invalid zip64 offset")
		}
		var z64 [56]byte
		if _, err := file.ReadAt(z64[:], int64(pos)); err != nil {
			return fmt.Errorf("cannot read zip64 EOCD")
		}
		if !bytes.Equal(z64[:4], []byte{'P', 'K', 0x06, 0x06}) ||
			!bytes.Equal(z64[16:24], make([]byte, 8)) ||
			!bytes.Equal(z64[24:32], z64[32:40]) {
			return fmt.Errorf("invalid zip64 EOCD")
		}
		recordSize := binary.LittleEndian.Uint64(z64[4:12])
		if recordSize < 44 || recordSize > 4096 || pos+12+recordSize != eocdPos-20 {
			return fmt.Errorf("invalid zip64 record size")
		}
		count = binary.LittleEndian.Uint64(z64[32:40])
		cdSize = binary.LittleEndian.Uint64(z64[40:48])
		cdOffset = binary.LittleEndian.Uint64(z64[48:56])
	}
	if count == 0 || count > uint64(maxReleaseFiles*2) || cdSize > maxCentralDirBytes ||
		cdOffset+cdSize < cdOffset || cdOffset+cdSize > eocdPos {
		return fmt.Errorf("zip central directory bounds exceeded")
	}
	return nil
}

func parseReleaseSums(text string) (map[string]string, error) {
	sums := make(map[string]string)
	seenLower := make(map[string]bool)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		hash, path, ok := strings.Cut(line, "  ")
		if !ok || !isLowerHexSHA256(hash) || path == releaseSHA256SumsPath {
			return nil, fmt.Errorf("invalid sha256Sums line format")
		}
		if err := classifyReleaseProgramPath(path); err != nil {
			return nil, err
		}
		lower := strings.ToLower(path)
		if seenLower[lower] {
			return nil, fmt.Errorf("duplicate or case-conflicting path in sha256Sums: %s", path)
		}
		seenLower[lower] = true
		sums[path] = hash
		if len(sums) > maxReleaseFiles {
			return nil, fmt.Errorf("sha256Sums file count exceeds limit")
		}
	}
	for p := range sums {
		parts := strings.Split(p, "/")
		prefix := ""
		for i := 0; i < len(parts)-1; i++ {
			if prefix != "" {
				prefix += "/"
			}
			prefix += parts[i]
			if seenLower[strings.ToLower(prefix)] {
				return nil, fmt.Errorf("file/directory conflict in sha256Sums: %s", prefix)
			}
		}
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("empty sha256Sums")
	}
	return sums, nil
}

func validateSafeReleasePath(path string) error {
	if path == "" || len(utf16.Encode([]rune(path))) > 240 || strings.Contains(path, "\\") {
		return fmt.Errorf("invalid release path")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(":*?\"<>|", r) {
			return fmt.Errorf("release path contains forbidden character")
		}
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return fmt.Errorf("release path contains unsafe component")
		}
		stem := strings.ToUpper(strings.TrimRight(strings.SplitN(part, ".", 2)[0], " "))
		switch stem {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return fmt.Errorf("release path contains reserved Windows device name")
		}
		for _, prefix := range []string{"COM", "LPT"} {
			if strings.HasPrefix(stem, prefix) {
				rest := strings.TrimPrefix(stem, prefix)
				switch rest {
				case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
					return fmt.Errorf("release path contains reserved Windows device name")
				}
			}
		}
	}
	return nil
}

func rejectRetiredReleasePath(path string) error {
	lower := strings.ToLower(path)
	for _, prefix := range []string{"wiztree", "app/tools/wiztree"} {
		if lower == prefix || strings.HasPrefix(lower, prefix+"/") {
			return fmt.Errorf("release contains retired path: %s", path)
		}
	}
	return nil
}

func classifyReleaseProgramPath(path string) error {
	if err := validateSafeReleasePath(path); err != nil {
		return err
	}
	if err := rejectRetiredReleasePath(path); err != nil {
		return err
	}
	lower := strings.ToLower(path)
	for _, part := range strings.Split(lower, "/") {
		switch part {
		case ".state", ".git", ".cache", "node-compile-cache", "target", "src", "test", "tests", "obj",
			".env", "auth.json", "models.json", "mcp-auth.json", "device.json", "web.json",
			"web-settings.json", "desktop.json", "desktop-settings.json", "tui.json", "identity.json", "agents.md":
			return fmt.Errorf("release contains runtime or dev file: %s", path)
		}
		if strings.HasPrefix(part, ".env.") {
			return fmt.Errorf("release contains env file: %s", path)
		}
	}
	if strings.HasSuffix(lower, ".otf") && path != "app/fonts/SourceHanSansCN-VF.otf" {
		return fmt.Errorf("release contains unapproved .otf font: %s", path)
	}
	if mergeReleaseFiles[path] {
		return fmt.Errorf("release cannot include user data path: %s", path)
	}
	switch path {
	case "cst-pilot.exe", "VERSION", "README.md", "CHANGELOG.md", "LICENSE", "THIRD-PARTY-NOTICES.md":
		return nil
	}
	if strings.HasPrefix(path, "app/") || (!strings.Contains(path, "/") && strings.HasSuffix(lower, ".dll")) {
		return nil
	}
	return fmt.Errorf("release path outside program whitelist: %s", path)
}

func checkPathSpelling(path string, spellings map[string]string) error {
	prefix := ""
	for _, part := range strings.Split(path, "/") {
		if prefix != "" {
			prefix += "/"
		}
		prefix += part
		key := strings.ToLower(prefix)
		if old, exists := spellings[key]; exists && old != prefix {
			return fmt.Errorf("case conflict on ancestor path: %s vs %s", old, prefix)
		}
		spellings[key] = prefix
	}
	return nil
}

func isLowerHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func checkJSONNoSecrets(v any) error {
	switch node := v.(type) {
	case map[string]any:
		for k, val := range node {
			norm := strings.ReplaceAll(strings.ToLower(k), "_", "")
			if forbiddenJSONSecretKeys[norm] {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					return fmt.Errorf("contains non-empty credential key %q", k)
				}
			}
			if err := checkJSONNoSecrets(val); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range node {
			if err := checkJSONNoSecrets(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStrictJSON 拒绝重复键、尾随数据及过深嵌套。
func validateStrictJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkStrictJSONToken(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON data")
	}
	return nil
}

func walkStrictJSONToken(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := kTok.(string)
			if !ok {
				return fmt.Errorf("expected string key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = true
			if err := walkStrictJSONToken(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("unterminated JSON object")
		}
	case '[':
		for dec.More() {
			if err := walkStrictJSONToken(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %v", delim)
	}
	return nil
}

func decodeStrictJSONStruct(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON tokens")
	}
	return nil
}

// -----------------------------------------------------------------------------
// SemVer 2.0 解析与比较（支持可选前导 v，对齐 Rust semver::Version）
// -----------------------------------------------------------------------------

type semverVersion struct {
	major uint64
	minor uint64
	patch uint64
	pre   []string
}

func parseSemver(raw string) (semverVersion, error) {
	if len(raw) == 0 || len(raw) > 64 || strings.TrimSpace(raw) != raw {
		return semverVersion{}, fmt.Errorf("invalid semver")
	}
	s := strings.TrimPrefix(raw, "v")
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		if idx == len(s)-1 {
			return semverVersion{}, fmt.Errorf("empty build metadata")
		}
		s = s[:idx]
	}
	var pre []string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		preStr := s[idx+1:]
		if preStr == "" {
			return semverVersion{}, fmt.Errorf("empty prerelease")
		}
		pre = strings.Split(preStr, ".")
		for _, id := range pre {
			if id == "" {
				return semverVersion{}, fmt.Errorf("empty prerelease identifier")
			}
			isNum := true
			for i := 0; i < len(id); i++ {
				c := id[i]
				if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '-') {
					return semverVersion{}, fmt.Errorf("invalid prerelease char")
				}
				if c < '0' || c > '9' {
					isNum = false
				}
			}
			if isNum && len(id) > 1 && id[0] == '0' {
				return semverVersion{}, fmt.Errorf("leading zero in numeric prerelease")
			}
		}
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semverVersion{}, fmt.Errorf("semver must have major.minor.patch")
	}
	nums := make([]uint64, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semverVersion{}, fmt.Errorf("invalid numeric version segment")
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semverVersion{}, err
		}
		nums[i] = n
	}
	return semverVersion{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, nil
}

func compareSemver(a, b semverVersion) int {
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return -1
		}
		return 1
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return -1
		}
		return 1
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	minLen := len(a.pre)
	if len(b.pre) < minLen {
		minLen = len(b.pre)
	}
	for i := 0; i < minLen; i++ {
		if a.pre[i] == b.pre[i] {
			continue
		}
		na, erra := strconv.ParseUint(a.pre[i], 10, 64)
		nb, errb := strconv.ParseUint(b.pre[i], 10, 64)
		switch {
		case erra == nil && errb == nil:
			if na < nb {
				return -1
			}
			return 1
		case erra == nil:
			return -1
		case errb == nil:
			return 1
		default:
			if a.pre[i] < b.pre[i] {
				return -1
			}
			return 1
		}
	}
	if len(a.pre) < len(b.pre) {
		return -1
	}
	if len(a.pre) > len(b.pre) {
		return 1
	}
	return 0
}

// -----------------------------------------------------------------------------
// 公网 IP 校验（对齐 cst-updater::manifest::validate_public_ip）
// -----------------------------------------------------------------------------

func rejectNonPublicIPHost(host string) error {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil // 域名由上层及 DNS 约束处理
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if addr.Is4() {
		b := addr.As4()
		if isPublicIPv4(b) {
			return nil
		}
		return fmt.Errorf("non-public IPv4 address rejected")
	}
	if addr.Is6() {
		b := addr.As16()
		if isPublicIPv6(b) {
			return nil
		}
		return fmt.Errorf("non-public IPv6 address rejected")
	}
	return fmt.Errorf("invalid IP host")
}

func isPublicIPv4(b [4]byte) bool {
	a, c2, c3 := b[0], b[1], b[2]
	return !(a == 0 ||
		a == 10 ||
		a == 127 ||
		a >= 224 ||
		(a == 100 && c2 >= 64 && c2 <= 127) ||
		(a == 169 && c2 == 254) ||
		(a == 172 && c2 >= 16 && c2 <= 31) ||
		(a == 192 && c2 == 168) ||
		(a == 192 && c2 == 0 && (c3 == 0 || c3 == 2)) ||
		(a == 192 && c2 == 88 && c3 == 99) ||
		(a == 198 && (c2 == 18 || c2 == 19)) ||
		(a == 198 && c2 == 51 && c3 == 100) ||
		(a == 203 && c2 == 0 && c3 == 113))
}

func isPublicIPv6(b [16]byte) bool {
	s0 := binary.BigEndian.Uint16(b[0:2])
	s1 := binary.BigEndian.Uint16(b[2:4])
	return (s0&0xe000) == 0x2000 &&
		!(s0 == 0x2001 && s1 < 0x0200) &&
		!(s0 == 0x2001 && s1 == 0x0db8) &&
		s0 != 0x2002 &&
		!(s0 == 0x3fff && (s1&0xf000) == 0)
}

// -----------------------------------------------------------------------------
// Ed25519 严格公钥与签名校验（数学等价于 ed25519-dalek::is_weak + verify_strict）
// -----------------------------------------------------------------------------

var (
	edP        *big.Int // 2^255 - 19
	edL        *big.Int // 2^252 + 27742317777372353535851937790883648493
	edD        *big.Int // -121665 / 121666 mod p
	edSqrtM1   *big.Int // 2^((p-1)/4) mod p
	edPPlus3D8 *big.Int // (p + 3) / 8
	edInitOnce sync.Once
)

func initEd25519Math() {
	edInitOnce.Do(func() {
		one := big.NewInt(1)
		edP = new(big.Int).Sub(new(big.Int).Lsh(one, 255), big.NewInt(19))
		edL, _ = new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
		num := new(big.Int).Mod(big.NewInt(-121665), edP)
		denInv := new(big.Int).ModInverse(big.NewInt(121666), edP)
		edD = new(big.Int).Mod(new(big.Int).Mul(num, denInv), edP)
		exp := new(big.Int).Rsh(new(big.Int).Sub(edP, one), 2)
		edSqrtM1 = new(big.Int).Exp(big.NewInt(2), exp, edP)
		edPPlus3D8 = new(big.Int).Rsh(new(big.Int).Add(edP, big.NewInt(3)), 3)
	})
}

type edPoint struct {
	x, y *big.Int
}

func decodeCanonicalEd25519Point(encoded []byte) (edPoint, error) {
	initEd25519Math()
	if len(encoded) != 32 {
		return edPoint{}, fmt.Errorf("point must be 32 bytes")
	}
	var le [32]byte
	copy(le[:], encoded)
	signBit := (le[31] >> 7) & 1
	le[31] &= 0x7f
	// 小端转大端供 big.Int 解析。
	var be [32]byte
	for i := 0; i < 32; i++ {
		be[i] = le[31-i]
	}
	y := new(big.Int).SetBytes(be[:])
	if y.Cmp(edP) >= 0 {
		return edPoint{}, fmt.Errorf("non-canonical point y >= p")
	}

	// x^2 = (y^2 - 1) / (d*y^2 + 1) mod p
	y2 := new(big.Int).Mod(new(big.Int).Mul(y, y), edP)
	u := new(big.Int).Mod(new(big.Int).Sub(y2, big.NewInt(1)), edP)
	v := new(big.Int).Mod(new(big.Int).Add(new(big.Int).Mul(edD, y2), big.NewInt(1)), edP)
	vInv := new(big.Int).ModInverse(v, edP)
	if vInv == nil {
		return edPoint{}, fmt.Errorf("invalid curve denominator")
	}
	x2 := new(big.Int).Mod(new(big.Int).Mul(u, vInv), edP)

	var x *big.Int
	if x2.Sign() == 0 {
		if signBit == 1 {
			return edPoint{}, fmt.Errorf("non-canonical zero x with sign bit")
		}
		x = big.NewInt(0)
	} else {
		x = new(big.Int).Exp(x2, edPPlus3D8, edP)
		check := new(big.Int).Mod(new(big.Int).Mul(x, x), edP)
		if check.Cmp(x2) != 0 {
			x.Mod(new(big.Int).Mul(x, edSqrtM1), edP)
			check = new(big.Int).Mod(new(big.Int).Mul(x, x), edP)
			if check.Cmp(x2) != 0 {
				return edPoint{}, fmt.Errorf("point not on edwards25519 curve")
			}
		}
		if uint8(x.Bit(0)) != signBit {
			x.Sub(edP, x)
		}
	}
	return edPoint{x: x, y: y}, nil
}

func edPointAdd(p1, p2 edPoint) edPoint {
	// Edwards25519 (a = -1):
	// x3 = (x1*y2 + y1*x2) / (1 + d*x1*x2*y1*y2)
	// y3 = (y1*y2 + x1*x2) / (1 - d*x1*x2*y1*y2)
	x1y2 := new(big.Int).Mod(new(big.Int).Mul(p1.x, p2.y), edP)
	y1x2 := new(big.Int).Mod(new(big.Int).Mul(p1.y, p2.x), edP)
	y1y2 := new(big.Int).Mod(new(big.Int).Mul(p1.y, p2.y), edP)
	x1x2 := new(big.Int).Mod(new(big.Int).Mul(p1.x, p2.x), edP)

	dxxyy := new(big.Int).Mod(new(big.Int).Mul(edD, new(big.Int).Mod(new(big.Int).Mul(x1x2, y1y2), edP)), edP)
	one := big.NewInt(1)
	denX := new(big.Int).ModInverse(new(big.Int).Mod(new(big.Int).Add(one, dxxyy), edP), edP)
	denY := new(big.Int).ModInverse(new(big.Int).Mod(new(big.Int).Sub(one, dxxyy), edP), edP)

	x3 := new(big.Int).Mod(new(big.Int).Mul(new(big.Int).Add(x1y2, y1x2), denX), edP)
	y3 := new(big.Int).Mod(new(big.Int).Mul(new(big.Int).Add(y1y2, x1x2), denY), edP)
	return edPoint{x: x3, y: y3}
}

func isSmallOrderEd25519Point(p edPoint) bool {
	p2 := edPointAdd(p, p)
	p4 := edPointAdd(p2, p2)
	p8 := edPointAdd(p4, p4)
	return p8.x.Sign() == 0 && p8.y.Cmp(big.NewInt(1)) == 0
}

func validateStrictEd25519PublicKey(pub ed25519.PublicKey) error {
	p, err := decodeCanonicalEd25519Point(pub)
	if err != nil {
		return err
	}
	if isSmallOrderEd25519Point(p) {
		return fmt.Errorf("weak small-order ed25519 public key rejected")
	}
	return nil
}

func verifyStrictEd25519Domain(pub ed25519.PublicKey, domain string, data []byte, encodedSig string) error {
	if len(encodedSig) == 0 || len(encodedSig) > 128 {
		return fmt.Errorf("invalid signature length")
	}
	sig, err := base64.StdEncoding.DecodeString(encodedSig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid base64 signature")
	}
	if err := validateStrictEd25519PublicKey(pub); err != nil {
		return err
	}
	// 严格校验 R 点规范编码且非小阶点。
	rPoint, err := decodeCanonicalEd25519Point(sig[:32])
	if err != nil {
		return fmt.Errorf("invalid signature R point: %w", err)
	}
	if isSmallOrderEd25519Point(rPoint) {
		return fmt.Errorf("small-order signature R point rejected")
	}
	// 严格校验标量 S < L。
	var sBE [32]byte
	for i := 0; i < 32; i++ {
		sBE[i] = sig[63-i]
	}
	sScalar := new(big.Int).SetBytes(sBE[:])
	if sScalar.Cmp(edL) >= 0 {
		return fmt.Errorf("non-canonical signature scalar S")
	}
	msg := make([]byte, 0, len(domain)+len(data))
	msg = append(msg, domain...)
	msg = append(msg, data...)
	// 复用标准库快速验签，同时拒绝非规范点/小阶点后与 verify_strict 保持一致。
	if !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("ed25519 signature mismatch")
	}
	// 验证无余因子等式 [S]B == R + [k]A（排除余因子消去伪造）。
	h := sha512.New()
	h.Write(sig[:32])
	h.Write(pub)
	h.Write(msg)
	var digest [64]byte
	h.Sum(digest[:0])
	var kBE [64]byte
	for i := 0; i < 64; i++ {
		kBE[i] = digest[63-i]
	}
	k := new(big.Int).Mod(new(big.Int).SetBytes(kBE[:]), edL)
	aPoint, err := decodeCanonicalEd25519Point(pub)
	if err != nil {
		return err
	}
	lhs := edScalarBaseMult(sScalar)
	rhs := edPointAdd(rPoint, edScalarMult(aPoint, k))
	if lhs.x.Cmp(rhs.x) != 0 || lhs.y.Cmp(rhs.y) != 0 {
		return fmt.Errorf("strict ed25519 equation mismatch")
	}
	return nil
}

func edScalarBaseMult(s *big.Int) edPoint {
	// RFC 8032 base point B: y = 4/5 mod p, x > 0 (even)
	four := big.NewInt(4)
	fiveInv := new(big.Int).ModInverse(big.NewInt(5), edP)
	by := new(big.Int).Mod(new(big.Int).Mul(four, fiveInv), edP)
	bx, _ := new(big.Int).SetString("15112221349535400772501151409588531511454012693041857206046113283949847762202", 10)
	return edScalarMult(edPoint{x: bx, y: by}, s)
}

func edScalarMult(p edPoint, scalar *big.Int) edPoint {
	res := edPoint{x: big.NewInt(0), y: big.NewInt(1)}
	addend := p
	for i := 0; i < scalar.BitLen(); i++ {
		if scalar.Bit(i) == 1 {
			res = edPointAdd(res, addend)
		}
		addend = edPointAdd(addend, addend)
	}
	return res
}
