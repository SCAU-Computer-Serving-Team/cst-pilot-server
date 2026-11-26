package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testReleaseBaseURL    = "https://www.cstoa.top/api/agent/releases"
	testReleaseAdminToken = "test-admin-token-0123456789abcdef0123456789abcdef"
)

type testReleaseBundle struct {
	version       string
	filename      string
	manifestBytes []byte
	archiveBytes  []byte
	archiveSHA256 string
}

func makeMinimalValidSFNT(magic [4]byte) []byte {
	// 12 byte offset table + 2 table records (cmap, name) = 44 bytes + 16 bytes payload = 60 bytes
	buf := make([]byte, 60)
	copy(buf[0:4], magic[:])
	binary.BigEndian.PutUint16(buf[4:6], 2) // numTables = 2
	// Table 0: cmap at offset 44, len 8
	copy(buf[12:16], "cmap")
	binary.BigEndian.PutUint32(buf[20:24], 44)
	binary.BigEndian.PutUint32(buf[24:28], 8)
	// Table 1: name at offset 52, len 8
	copy(buf[28:32], "name")
	binary.BigEndian.PutUint32(buf[36:40], 52)
	binary.BigEndian.PutUint32(buf[40:44], 8)
	return buf
}

func buildValidReleaseBundle(t *testing.T, priv ed25519.PrivateKey, version, minVer, baseURL string, extraFiles map[string][]byte, mutateManifest func(*releaseManifest)) testReleaseBundle {
	t.Helper()
	files := map[string][]byte{
		"cst-pilot.exe":                       []byte("MZ-pilot-exe-" + version),
		"app/cst-server.exe":                  []byte("MZ-server-exe-" + version),
		"app/cst-updater.exe":                 []byte("MZ-updater-exe-" + version),
		"VERSION":                             []byte("cst-pilot " + version + "\n"),
		"app/BUILD-INFO.json":                 []byte(fmt.Sprintf(`{"version":%q,"platform":"windows-x86_64"}`, version)),
		"LICENSE":                             []byte("MIT License\n"),
		"THIRD-PARTY-NOTICES.md":              []byte("# Notices\n"),
		"app/prompts/APPEND_SYSTEM.md":        []byte("# Prompt\n"),
		"app/telemetry/timserver_1.crt":       []byte("-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n"),
		"app/telemetry/currency.json":         []byte(`{"CNY":1}`),
		"app/defaults/models-store.json":      []byte(`{}`),
		"app/defaults/telemetry.json":         []byte(`{}`),
		"app/defaults/web-search.json":        []byte(`{}`),
		"app/defaults/settings.json":          []byte(`{}`),
		"app/fonts/SourceHanSansCN-VF.otf":    makeMinimalValidSFNT([4]byte{'O', 'T', 'T', 'O'}),
		"app/fonts/LICENSE.txt":               []byte("OFL\n"),
		"app/fonts/JetBrainsMono-Regular.ttf": makeMinimalValidSFNT([4]byte{0, 1, 0, 0}),
		"app/fonts/JetBrainsMono-Medium.ttf":  makeMinimalValidSFNT([4]byte{0, 1, 0, 0}),
		"app/fonts/JetBrainsMono-OFL.txt":     []byte("OFL\n"),
		"app/licenses/ntfs-ng-LICENSE.txt":    []byte("Apache-2.0\n"),
	}
	for _, skill := range requiredReleaseSkills {
		files["app/skills/"+skill+"/SKILL.md"] = []byte("# " + skill + "\n")
	}
	for k, v := range extraFiles {
		files[k] = v
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var sumsBuilder strings.Builder
	var unpacked uint64
	for _, p := range paths {
		h := sha256.Sum256(files[p])
		sumsBuilder.WriteString(hex.EncodeToString(h[:]))
		sumsBuilder.WriteString("  ")
		sumsBuilder.WriteString(p)
		sumsBuilder.WriteByte('\n')
		unpacked += uint64(len(files[p]))
	}
	sumsText := sumsBuilder.String()
	// unpackedSize 包含 app/SHA256SUMS 自身字节数。
	unpacked += uint64(len(sumsText))

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	allZipPaths := append(append([]string(nil), paths...), releaseSHA256SumsPath)
	sort.Strings(allZipPaths)
	for _, p := range allZipPaths {
		w, err := zw.Create("cst-pilot/" + p)
		if err != nil {
			t.Fatal(err)
		}
		data := files[p]
		if p == releaseSHA256SumsPath {
			data = []byte(sumsText)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archiveBytes := zipBuf.Bytes()
	archiveSum := sha256.Sum256(archiveBytes)
	archiveHex := hex.EncodeToString(archiveSum[:])

	sumsSig := ed25519.Sign(priv, append([]byte(sumsDomain), []byte(sumsText)...))
	manifest := releaseManifest{
		SchemaVersion:        1,
		Platform:             "windows-x86_64",
		Version:              version,
		BuiltAt:              "2026-11-26T09:00:00Z",
		MinUpgradableVersion: minVer,
		ReleaseNotes:         "Release notes for " + version,
		Archive: releaseArchiveMeta{
			URL:          expectedReleaseArchiveURL(baseURL, version),
			Size:         uint64(len(archiveBytes)),
			UnpackedSize: unpacked,
			SHA256:       archiveHex,
		},
		SHA256Sums:      sumsText,
		SHA256Signature: base64.StdEncoding.EncodeToString(sumsSig),
	}
	if mutateManifest != nil {
		mutateManifest(&manifest)
	}
	payloadBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestSig := ed25519.Sign(priv, append([]byte(manifestDomain), payloadBytes...))
	envBytes, err := json.Marshal(signedManifestEnvelope{
		Payload:   string(payloadBytes),
		Signature: base64.StdEncoding.EncodeToString(manifestSig),
	})
	if err != nil {
		t.Fatal(err)
	}
	return testReleaseBundle{
		version:       version,
		filename:      expectedReleaseArchiveFilename(version),
		manifestBytes: envBytes,
		archiveBytes:  archiveBytes,
		archiveSHA256: archiveHex,
	}
}

func makeMultipartPublishRequest(t *testing.T, urlPath, token string, manifestBytes, archiveBytes []byte, archiveName string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p1, err := mw.CreateFormFile("manifest", "manifest.signed.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p1.Write(manifestBytes); err != nil {
		t.Fatal(err)
	}
	p2, err := mw.CreateFormFile("archive", archiveName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Write(archiveBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, urlPath, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = "127.0.0.1:50000"
	return req
}

func newTestReleaseEnv(t *testing.T, mutateCfg func(*releaseConfig), freeFn func(string) (uint64, error), nowFn func() time.Time) (*http.Server, *releaseStore, *releaseGuard, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := releaseConfig{
		Enabled:            true,
		StorageDir:         filepath.Join(t.TempDir(), "releases"),
		PublicKey:          pub,
		PublicBaseURL:      testReleaseBaseURL,
		AdminToken:         testReleaseAdminToken,
		MaxArchiveBytes:    defaultReleaseMaxArchiveBytes,
		MinFreeBytes:       1 << 20, // 单测默认 1 MiB，可用 freeFn 精确控制水位
		KeepCount:          5,
		MaxTotalBytes:      defaultReleaseMaxTotalBytes,
		HourlyBudgetBytes:  defaultReleaseHourlyBudgetBytes,
		DailyBudgetBytes:   defaultReleaseDailyBudgetBytes,
		MaxDirectDownloads: 4,
	}
	if mutateCfg != nil {
		mutateCfg(&cfg)
	}
	if freeFn == nil {
		freeFn = func(string) (uint64, error) { return 20 << 30, nil }
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	relStore, err := openReleaseStoreWithHooks(cfg, nowFn, freeFn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relStore.close() })
	relGuard := newReleaseGuard(cfg, nowFn)
	telStore := testStore(t)
	srv := newTelemetryServer(serverOptions{
		store:        telStore,
		auth:         newIntrospector(authOptions{AllowStub: true}),
		releases:     relStore,
		releaseGuard: relGuard,
		now:          nowFn,
	})
	return srv, relStore, relGuard, priv
}

func TestReleaseModuleDisabledZeroSideEffects(t *testing.T) {
	st := testStore(t)
	srv := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{AllowStub: true})})

	for _, path := range []string{
		"/v1/releases/channels/stable/latest",
		"/v1/releases/files/v0.9.6/cst-pilot-v0.9.6-windows-x64.zip",
		"/v1/admin/releases",
	} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s expected 404 when disabled, got %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"releases"`) {
		t.Fatalf("healthz unexpected when disabled: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReleaseConfigRejectsWeakKeyShortTokenAndBadURL(t *testing.T) {
	// 8 阶扭点之一（identity 点 0x01, 0x00...）必须被拒绝。
	identityPoint := make([]byte, 32)
	identityPoint[0] = 1
	if _, err := parseStrictReleasePublicKey(base64.StdEncoding.EncodeToString(identityPoint)); err == nil {
		t.Fatal("expected identity weak ed25519 key to be rejected")
	}
	order2Point := make([]byte, 32)
	order2Point[0] = 0xec
	for i := 1; i < 31; i++ {
		order2Point[i] = 0xff
	}
	order2Point[31] = 0x7f
	if _, err := parseStrictReleasePublicKey(base64.StdEncoding.EncodeToString(order2Point)); err == nil {
		t.Fatal("expected order-2 weak ed25519 key to be rejected")
	}

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	baseCfg := releaseConfig{
		Enabled:            true,
		StorageDir:         t.TempDir(),
		PublicKey:          pub,
		PublicBaseURL:      testReleaseBaseURL,
		AdminToken:         "too-short",
		MaxArchiveBytes:    defaultReleaseMaxArchiveBytes,
		KeepCount:          5,
		MaxTotalBytes:      defaultReleaseMaxTotalBytes,
		HourlyBudgetBytes:  defaultReleaseHourlyBudgetBytes,
		DailyBudgetBytes:   defaultReleaseDailyBudgetBytes,
		MaxDirectDownloads: 4,
	}
	if err := validateReleaseConfig(baseCfg); err == nil {
		t.Fatal("expected short admin token to be rejected")
	}
	baseCfg.AdminToken = testReleaseAdminToken
	for _, badURL := range []string{
		"http://www.cstoa.top/api/agent/releases",
		"https://127.0.0.1/api/agent/releases",
		"https://192.168.1.1/api/agent/releases",
		"https://user:pass@www.cstoa.top/api/agent/releases",
		"https://www.cstoa.top/api/agent/releases#frag",
	} {
		baseCfg.PublicBaseURL = badURL
		if err := validateReleaseConfig(baseCfg); err == nil {
			t.Fatalf("expected invalid base url %q to be rejected", badURL)
		}
	}
}

func TestReleaseEndToEndPublishAndDualModeDownload(t *testing.T) {
	srv, _, _, priv := newTestReleaseEnv(t, nil, nil, nil)

	// 空通道返回 404 channel_empty。
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/releases/channels/stable/latest", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"channel_empty"`) {
		t.Fatalf("empty channel = %d %s", rec.Code, rec.Body.String())
	}

	bundle := buildValidReleaseBundle(t, priv, "v0.9.6", "v0.9.0", testReleaseBaseURL, nil, nil)

	// 发布 v0.9.6 至 stable。
	pubRec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(pubRec, makeMultipartPublishRequest(t, "/v1/admin/releases?channel=stable&activate=true", testReleaseAdminToken, bundle.manifestBytes, bundle.archiveBytes, bundle.filename))
	if pubRec.Code != http.StatusCreated {
		t.Fatalf("publish failed: %d %s", pubRec.Code, pubRec.Body.String())
	}

	// 同版本不可覆盖。
	dupRec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(dupRec, makeMultipartPublishRequest(t, "/v1/admin/releases?channel=stable&activate=true", testReleaseAdminToken, bundle.manifestBytes, bundle.archiveBytes, bundle.filename))
	if dupRec.Code != http.StatusConflict || !strings.Contains(dupRec.Body.String(), `"version_already_exists"`) {
		t.Fatalf("duplicate publish = %d %s", dupRec.Code, dupRec.Body.String())
	}

	// 清单查询原样返回，支持 ETag / 304。
	mRec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/v1/releases/channels/stable/latest", nil))
	if mRec.Code != http.StatusOK || !bytes.Equal(mRec.Body.Bytes(), bundle.manifestBytes) {
		t.Fatalf("manifest mismatch: code=%d", mRec.Code)
	}
	etag := mRec.Header().Get("ETag")
	if etag == "" || mRec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("manifest headers unexpected: etag=%q cc=%q", etag, mRec.Header().Get("Cache-Control"))
	}
	m304 := httptest.NewRecorder()
	req304 := httptest.NewRequest(http.MethodGet, "/v1/releases/channels/stable/latest", nil)
	req304.Header.Set("If-None-Match", etag)
	srv.Handler.ServeHTTP(m304, req304)
	if m304.Code != http.StatusNotModified {
		t.Fatalf("manifest 304 expected, got %d", m304.Code)
	}

	// Go 原生直出下载：200 OK、HEAD、ETag/304、Range/206。
	fileURL := "/v1/releases/files/" + bundle.version + "/" + bundle.filename
	headRec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(headRec, httptest.NewRequest(http.MethodHead, fileURL, nil))
	if headRec.Code != http.StatusOK || headRec.Body.Len() != 0 {
		t.Fatalf("HEAD archive = %d len=%d", headRec.Code, headRec.Body.Len())
	}

	dlRec := httptest.NewRecorder()
	dlReq := httptest.NewRequest(http.MethodGet, fileURL, nil)
	dlReq.RemoteAddr = "198.51.100.10:12345"
	srv.Handler.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK || !bytes.Equal(dlRec.Body.Bytes(), bundle.archiveBytes) {
		t.Fatalf("direct download = %d len=%d want=%d", dlRec.Code, dlRec.Body.Len(), len(bundle.archiveBytes))
	}
	fileETag := dlRec.Header().Get("ETag")
	dl304 := httptest.NewRecorder()
	reqF304 := httptest.NewRequest(http.MethodGet, fileURL, nil)
	reqF304.Header.Set("If-None-Match", fileETag)
	srv.Handler.ServeHTTP(dl304, reqF304)
	if dl304.Code != http.StatusNotModified {
		t.Fatalf("archive 304 expected, got %d", dl304.Code)
	}

	rangeRec := httptest.NewRecorder()
	rangeReq := httptest.NewRequest(http.MethodGet, fileURL, nil)
	rangeReq.RemoteAddr = "198.51.100.10:12346"
	rangeReq.Header.Set("Range", "bytes=0-15")
	srv.Handler.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent || !bytes.Equal(rangeRec.Body.Bytes(), bundle.archiveBytes[:16]) {
		t.Fatalf("range download = %d len=%d", rangeRec.Code, rangeRec.Body.Len())
	}

	// /healthz 附带 releases 摘要。
	hz := httptest.NewRecorder()
	srv.Handler.ServeHTTP(hz, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if hz.Code != 200 || !strings.Contains(hz.Body.String(), `"stableVersion":"v0.9.6"`) {
		t.Fatalf("healthz missing releases summary: %s", hz.Body.String())
	}

	// Nginx X-Accel-Redirect 模式。
	srvAccel, _, _, priv2 := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.AccelPrefix = "/_internal/cst-releases"
	}, nil, nil)
	b2 := buildValidReleaseBundle(t, priv2, "v0.9.7", "v0.9.0", testReleaseBaseURL, nil, nil)
	rPub := httptest.NewRecorder()
	srvAccel.Handler.ServeHTTP(rPub, makeMultipartPublishRequest(t, "/v1/admin/releases?channel=stable&activate=true", testReleaseAdminToken, b2.manifestBytes, b2.archiveBytes, b2.filename))
	if rPub.Code != http.StatusCreated {
		t.Fatalf("accel publish failed: %d %s", rPub.Code, rPub.Body.String())
	}
	rAccel := httptest.NewRecorder()
	reqAccel := httptest.NewRequest(http.MethodGet, "/v1/releases/files/"+b2.version+"/"+b2.filename, nil)
	reqAccel.RemoteAddr = "198.51.100.11:2222"
	srvAccel.Handler.ServeHTTP(rAccel, reqAccel)
	wantRedirect := "/_internal/cst-releases/v0.9.7/" + b2.filename
	if rAccel.Code != http.StatusOK || rAccel.Body.Len() != 0 || rAccel.Header().Get("X-Accel-Redirect") != wantRedirect {
		t.Fatalf("accel download = %d redirect=%q body=%d", rAccel.Code, rAccel.Header().Get("X-Accel-Redirect"), rAccel.Body.Len())
	}
}

func TestReleaseVerificationRejectsTamperingAndRedlines(t *testing.T) {
	srv, _, _, priv := newTestReleaseEnv(t, nil, nil, nil)

	// 1. 篡改清单签名的一字节。
	b1 := buildValidReleaseBundle(t, priv, "v1.0.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	var env signedManifestEnvelope
	_ = json.Unmarshal(b1.manifestBytes, &env)
	sigBytes, _ := base64.StdEncoding.DecodeString(env.Signature)
	sigBytes[0] ^= 0xff
	env.Signature = base64.StdEncoding.EncodeToString(sigBytes)
	badSigEnv, _ := json.Marshal(env)
	r1 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(r1, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, badSigEnv, b1.archiveBytes, b1.filename))
	if r1.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tampered signature expected 422, got %d %s", r1.Code, r1.Body.String())
	}

	// 2. 篡改 ZIP 一字节。
	badZip := append([]byte(nil), b1.archiveBytes...)
	badZip[len(badZip)-1] ^= 0x01
	r2 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(r2, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b1.manifestBytes, badZip, b1.filename))
	if r2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tampered zip expected 422, got %d %s", r2.Code, r2.Body.String())
	}

	// 3. unpackedSize 少算 app/SHA256SUMS 字节数。
	b3 := buildValidReleaseBundle(t, priv, "v1.0.1", "v0.9.0", testReleaseBaseURL, nil, func(m *releaseManifest) {
		m.Archive.UnpackedSize -= uint64(len(m.SHA256Sums))
	})
	r3 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(r3, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b3.manifestBytes, b3.archiveBytes, b3.filename))
	if r3.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unpackedSize mismatch expected 422, got %d %s", r3.Code, r3.Body.String())
	}

	// 4. 包含退役路径 app/tools/wiztree/wiztree.exe。
	b4 := buildValidReleaseBundle(t, priv, "v1.0.2", "v0.9.0", testReleaseBaseURL, map[string][]byte{
		"app/tools/wiztree/wiztree.exe": []byte("retired"),
	}, nil)
	r4 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(r4, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b4.manifestBytes, b4.archiveBytes, b4.filename))
	if r4.Code != http.StatusUnprocessableEntity {
		t.Fatalf("retired wiztree expected 422, got %d %s", r4.Code, r4.Body.String())
	}

	// 5. 包含 JSON 非空凭据键。
	b5 := buildValidReleaseBundle(t, priv, "v1.0.3", "v0.9.0", testReleaseBaseURL, map[string][]byte{
		"app/defaults/settings.json": []byte(`{"apiKey":"leak-secret"}`),
	}, nil)
	r5 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(r5, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b5.manifestBytes, b5.archiveBytes, b5.filename))
	if r5.Code != http.StatusUnprocessableEntity {
		t.Fatalf("json secret expected 422, got %d %s", r5.Code, r5.Body.String())
	}

	// 6. archive.url 非本服务规范同源前缀。
	b6 := buildValidReleaseBundle(t, priv, "v1.0.4", "v0.9.0", "https://evil.example/api/agent/releases", nil, nil)
	r6 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(r6, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b6.manifestBytes, b6.archiveBytes, b6.filename))
	if r6.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign archive url expected 422, got %d %s", r6.Code, r6.Body.String())
	}

	// 7. 严格 JSON 拒绝重复键。
	if err := validateStrictJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("expected duplicate JSON key to be rejected")
	}
}

func TestReleaseDiskWatermarkAndConcurrentPublishLock(t *testing.T) {
	// 磁盘空间低于水位线返回 507 且不消费 archive。
	srvLow, _, _, privLow := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.MinFreeBytes = 4 << 30
	}, func(string) (uint64, error) {
		return 1 << 30, nil // 仅剩 1 GiB
	}, nil)
	bLow := buildValidReleaseBundle(t, privLow, "v1.1.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	rLow := httptest.NewRecorder()
	srvLow.Handler.ServeHTTP(rLow, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, bLow.manifestBytes, bLow.archiveBytes, bLow.filename))
	if rLow.Code != http.StatusInsufficientStorage || !strings.Contains(rLow.Body.String(), `"insufficient_storage"`) {
		t.Fatalf("low disk expected 507, got %d %s", rLow.Code, rLow.Body.String())
	}

	// 并发上传触发独占锁返回 409 publish_in_progress。
	_, store, _, priv := newTestReleaseEnv(t, nil, nil, nil)
	bA := buildValidReleaseBundle(t, priv, "v1.2.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	bB := buildValidReleaseBundle(t, priv, "v1.2.1", "v0.9.0", testReleaseBaseURL, nil, nil)
	started := make(chan struct{})
	releaseGate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := store.publishStreaming(t.Context(), "stable", true, bA.manifestBytes, func() (io.Reader, error) {
			close(started)
			<-releaseGate
			return bytes.NewReader(bA.archiveBytes), nil
		}, nil, "test-a")
		if err != nil {
			t.Errorf("first publish failed: %v", err)
		}
	}()
	<-started
	_, err := store.publishStreaming(t.Context(), "stable", true, bB.manifestBytes, func() (io.Reader, error) {
		return bytes.NewReader(bB.archiveBytes), nil
	}, nil, "test-b")
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("expected publish_in_progress conflict, got %v", err)
	}
	close(releaseGate)
	wg.Wait()
}

func TestReleaseActivateRollbackGCProtectionAndRecovery(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
	srv, store, _, priv := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.KeepCount = 2
	}, nil, clock)

	// 依次发布 v1.0.0 -> v1.0.1 -> v1.0.2 (stable) 与 v1.1.0-beta.1 (beta)
	// KeepCount=2 但保护集合为 stable(v1.0.2)、previousStable(v1.0.1)、beta(v1.1.0-beta.1)，v1.0.0 被自动回收。
	for _, item := range []struct {
		ver string
		ch  string
	}{
		{"v1.0.0", "stable"},
		{"v1.0.1", "stable"},
		{"v1.0.2", "stable"},
		{"v1.1.0-beta.1", "beta"},
	} {
		b := buildValidReleaseBundle(t, priv, item.ver, "v0.9.0", testReleaseBaseURL, nil, nil)
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, makeMultipartPublishRequest(t, "/v1/admin/releases?channel="+item.ch+"&activate=true", testReleaseAdminToken, b.manifestBytes, b.archiveBytes, b.filename))
		if rec.Code != http.StatusCreated {
			t.Fatalf("publish %s failed: %d %s", item.ver, rec.Code, rec.Body.String())
		}
	}

	if _, ok := store.getFile("v1.0.0", expectedReleaseArchiveFilename("v1.0.0")); ok {
		t.Fatal("expected oldest unprotected version v1.0.0 to be GC evicted")
	}
	for _, kept := range []string{"v1.0.1", "v1.0.2", "v1.1.0-beta.1"} {
		if _, ok := store.getFile(kept, expectedReleaseArchiveFilename(kept)); !ok {
			t.Fatalf("expected protected version %s to remain stored", kept)
		}
	}

	// 回滚 stable 到 v1.0.1。
	rbReq := httptest.NewRequest(http.MethodPut, "/v1/admin/releases/channels/stable", strings.NewReader(`{"version":"v1.0.1"}`))
	rbReq.Header.Set("Authorization", "Bearer "+testReleaseAdminToken)
	rbRec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rbRec, rbReq)
	if rbRec.Code != http.StatusOK {
		t.Fatalf("rollback failed: %d %s", rbRec.Code, rbRec.Body.String())
	}
	if ch, ok := store.getChannel("stable"); !ok || ch.Version != "v1.0.1" {
		t.Fatalf("stable channel after rollback = %v %v", ok, ch)
	}

	// 激活版本不允许删除。
	delActiveReq := httptest.NewRequest(http.MethodDelete, "/v1/admin/releases/versions/v1.0.1", nil)
	delActiveReq.Header.Set("Authorization", "Bearer "+testReleaseAdminToken)
	delActiveRec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(delActiveRec, delActiveReq)
	if delActiveRec.Code != http.StatusConflict || !strings.Contains(delActiveRec.Body.String(), `"version_active"`) {
		t.Fatalf("delete active expected 409, got %d %s", delActiveRec.Code, delActiveRec.Body.String())
	}

	// 模拟崩溃残留：在 .tmp 和 versions 下放入孤儿目录，重新打开 store 应自动清理并保留有效激活版本。
	if err := os.MkdirAll(filepath.Join(store.tmpDir(), "upload-crashed"), 0o700); err != nil {
		t.Fatal(err)
	}
	orphanDir := filepath.Join(store.versionsDir(), "v9.9.9-orphan")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 关闭旧 store 释放生命周期锁后再重新打开，验证启动崩溃自愈。
	cfgCopy := store.cfg
	freeCopy := store.freeBytes
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openReleaseStoreWithHooks(cfgCopy, clock, freeCopy)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	defer recovered.close()
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatal("expected orphan directory to be cleaned on startup")
	}
	if entries, _ := os.ReadDir(recovered.tmpDir()); len(entries) != 0 {
		t.Fatalf("expected .tmp dir cleaned on startup, left %d entries", len(entries))
	}
	if ch, ok := recovered.getChannel("stable"); !ok || ch.Version != "v1.0.1" {
		t.Fatalf("recovered stable channel mismatch")
	}
}

func TestReleaseRateLimitBandwidthBudgetAndProxyIP(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// 先测单 IP 10 分钟 5 次下载上限与 TrustProxy。
	srv, _, _, priv := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.TrustProxy = true
		c.HourlyBudgetBytes = 100 << 20
		c.DailyBudgetBytes = 200 << 20
	}, nil, clock)
	b := buildValidReleaseBundle(t, priv, "v2.0.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	rPub := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rPub, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b.manifestBytes, b.archiveBytes, b.filename))
	if rPub.Code != http.StatusCreated {
		t.Fatalf("publish failed: %d", rPub.Code)
	}
	fileURL := "/v1/releases/files/" + b.version + "/" + b.filename
	for i := 0; i < fileDownloadMaxPerIP; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, fileURL, nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("X-Real-IP", "203.0.113.55")
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("download #%d expected 200, got %d", i+1, rec.Code)
		}
	}
	rec6 := httptest.NewRecorder()
	req6 := httptest.NewRequest(http.MethodGet, fileURL, nil)
	req6.RemoteAddr = "127.0.0.1:40001"
	req6.Header.Set("X-Real-IP", "203.0.113.55")
	srv.Handler.ServeHTTP(rec6, req6)
	if rec6.Code != http.StatusTooManyRequests || rec6.Header().Get("Retry-After") == "" || !strings.Contains(rec6.Body.String(), `"rate_limited"`) {
		t.Fatalf("6th download expected 429 rate_limited, got %d %s", rec6.Code, rec6.Body.String())
	}

	// 测试滚动 1 小时流量预算熔断并在 61 分钟后自动恢复。
	srvBudget, _, _, priv2 := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.TrustProxy = true
		c.HourlyBudgetBytes = int64(len(b.archiveBytes)) + 100 // 仅够 1 次完整下载，第 2 次必超限
		c.DailyBudgetBytes = int64(len(b.archiveBytes)) * 10
	}, nil, clock)
	b2 := buildValidReleaseBundle(t, priv2, "v2.0.1", "v0.9.0", testReleaseBaseURL, nil, nil)
	rPub2 := httptest.NewRecorder()
	srvBudget.Handler.ServeHTTP(rPub2, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, b2.manifestBytes, b2.archiveBytes, b2.filename))
	if rPub2.Code != http.StatusCreated {
		t.Fatalf("publish2 failed: %d", rPub2.Code)
	}
	u2 := "/v1/releases/files/" + b2.version + "/" + b2.filename
	recA := httptest.NewRecorder()
	reqA := httptest.NewRequest(http.MethodGet, u2, nil)
	reqA.RemoteAddr = "127.0.0.1:41000"
	reqA.Header.Set("X-Real-IP", "203.0.113.60")
	srvBudget.Handler.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("first budgeted download = %d", recA.Code)
	}
	recB := httptest.NewRecorder()
	reqB := httptest.NewRequest(http.MethodGet, u2, nil)
	reqB.RemoteAddr = "127.0.0.1:41001"
	reqB.Header.Set("X-Real-IP", "203.0.113.61")
	srvBudget.Handler.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusServiceUnavailable || recB.Header().Get("Retry-After") == "" || !strings.Contains(recB.Body.String(), `"bandwidth_budget_exceeded"`) {
		t.Fatalf("expected 503 bandwidth_budget_exceeded, got %d %s", recB.Code, recB.Body.String())
	}
	// 清单接口在熔断期间仍保持可读。
	recM := httptest.NewRecorder()
	srvBudget.Handler.ServeHTTP(recM, httptest.NewRequest(http.MethodGet, "/v1/releases/channels/stable/latest", nil))
	if recM.Code != http.StatusOK {
		t.Fatalf("manifest should remain readable during budget trip, got %d", recM.Code)
	}
	// 时钟推进 61 分钟后预算恢复。
	now = now.Add(61 * time.Minute)
	recC := httptest.NewRecorder()
	reqC := httptest.NewRequest(http.MethodGet, u2, nil)
	reqC.RemoteAddr = "127.0.0.1:41002"
	reqC.Header.Set("X-Real-IP", "203.0.113.62")
	srvBudget.Handler.ServeHTTP(recC, reqC)
	if recC.Code != http.StatusOK {
		t.Fatalf("expected download recovery after 61m, got %d %s", recC.Code, recC.Body.String())
	}

	// 管理鉴权连续失败达到阈值（第 10 次）即触发 429 锁定。
	for i := 0; i < adminAuthMaxFailures-1; i++ {
		badReq := httptest.NewRequest(http.MethodGet, "/v1/admin/releases", nil)
		badReq.RemoteAddr = "198.51.100.77:9999"
		badReq.Header.Set("Authorization", "Bearer wrong-token")
		badRec := httptest.NewRecorder()
		srvBudget.Handler.ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusUnauthorized {
			t.Fatalf("fail #%d expected 401, got %d", i+1, badRec.Code)
		}
	}
	badReq10 := httptest.NewRequest(http.MethodGet, "/v1/admin/releases", nil)
	badReq10.RemoteAddr = "198.51.100.77:9999"
	badReq10.Header.Set("Authorization", "Bearer wrong-token")
	badRec10 := httptest.NewRecorder()
	srvBudget.Handler.ServeHTTP(badRec10, badReq10)
	if badRec10.Code != http.StatusTooManyRequests || badRec10.Header().Get("Retry-After") == "" {
		t.Fatalf("10th failure expected 429 with Retry-After, got %d", badRec10.Code)
	}
	lockReq := httptest.NewRequest(http.MethodGet, "/v1/admin/releases", nil)
	lockReq.RemoteAddr = "198.51.100.77:9999"
	lockReq.Header.Set("Authorization", "Bearer "+testReleaseAdminToken)
	lockRec := httptest.NewRecorder()
	srvBudget.Handler.ServeHTTP(lockRec, lockReq)
	if lockRec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 while locked out, got %d", lockRec.Code)
	}
}

func TestReleaseOfflineCLIWorkflow(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	storageDir := filepath.Join(t.TempDir(), "cli-releases")
	t.Setenv("RELEASE_STORAGE_DIR", storageDir)
	t.Setenv("RELEASE_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("RELEASE_PUBLIC_BASE_URL", testReleaseBaseURL)
	t.Setenv("RELEASE_MIN_FREE_BYTES", "1048576")

	b := buildValidReleaseBundle(t, priv, "v3.0.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	workDir := t.TempDir()
	mPath := filepath.Join(workDir, "manifest.signed.json")
	aPath := filepath.Join(workDir, b.filename)
	if err := os.WriteFile(mPath, b.manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aPath, b.archiveBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := releaseCmd([]string{"publish", "--offline", "--manifest", mPath, "--archive", aPath, "--channel", "beta"}); err != nil {
		t.Fatalf("cli publish offline failed: %v", err)
	}
	if err := releaseCmd([]string{"activate", "--offline", "--channel", "stable", "--version", "v3.0.0"}); err != nil {
		t.Fatalf("cli activate offline failed: %v", err)
	}
	if err := releaseCmd([]string{"list", "--offline"}); err != nil {
		t.Fatalf("cli list offline failed: %v", err)
	}
	if err := releaseCmd([]string{"gc", "--offline"}); err != nil {
		t.Fatalf("cli gc offline failed: %v", err)
	}
	// CLI 拒绝向远端明文 HTTP 发送管理令牌。
	if err := releaseCmd([]string{"list", "--endpoint", "http://remote.example:8787"}); err == nil {
		t.Fatal("expected CLI to reject remote plaintext HTTP endpoint")
	}
}

func TestReleaseReviewRegressions(t *testing.T) {
	// P1-01：同一存储目录只允许一个生命周期所有者进程，第二个 open 必须被拒绝。
	_, store, _, priv := newTestReleaseEnv(t, nil, nil, nil)
	if _, err := openReleaseStoreWithHooks(store.cfg, store.now, store.freeBytes); err == nil {
		t.Fatal("expected second openReleaseStore on locked directory to fail")
	}

	// P1-02：Rename(state) 前失败回滚 targetDir，重试同版本成功；Rename(state) 后 fsync 失败时内存与磁盘仍同步。
	b1 := buildValidReleaseBundle(t, priv, "v4.0.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	store.beforeStateRename = func() error { return fmt.Errorf("injected before state rename") }
	if _, err := store.publishStreaming(t.Context(), "stable", true, b1.manifestBytes, func() (io.Reader, error) {
		return bytes.NewReader(b1.archiveBytes), nil
	}, nil, "test"); err == nil {
		t.Fatal("expected publish to fail on beforeStateRename")
	}
	if _, err := os.Stat(store.versionDir("v4.0.0")); !os.IsNotExist(err) {
		t.Fatal("expected target version dir rolled back when state rename did not happen")
	}
	store.beforeStateRename = nil
	store.afterStateRename = func() error { return fmt.Errorf("injected post-rename sync error") }
	if _, err := store.publishStreaming(t.Context(), "stable", true, b1.manifestBytes, func() (io.Reader, error) {
		return bytes.NewReader(b1.archiveBytes), nil
	}, nil, "test"); err == nil {
		t.Fatal("expected error surfaced from afterStateRename")
	}
	store.afterStateRename = nil
	if ch, ok := store.getChannel("stable"); !ok || ch.Version != "v4.0.0" {
		t.Fatalf("memory snapshot diverged from committed disk state after state rename: %v %v", ok, ch)
	}

	// P1-06：拒绝大小写模糊键、缺失/null 必填字段、未配对 UTF-16 代理项与非规范 Base64。
	if _, err := parseStrictSignedManifestEnvelope([]byte(`{"Payload":"x","signature":"y"}`)); err == nil {
		t.Fatal("expected case-mismatched key Payload to be rejected")
	}
	if _, err := parseStrictReleaseManifest([]byte(`{"schemaVersion":1,"platform":"windows-x86_64","version":"v1.0.0","builtAt":"2026-11-26T09:00:00Z","minUpgradableVersion":"v0.9.0","releaseNotes":null,"archive":{"url":"https://www.cstoa.top/a","size":1,"unpackedSize":1,"sha256":"` + strings.Repeat("a", 64) + `"},"sha256Sums":"x","sha256Signature":"y"}`)); err == nil {
		t.Fatal("expected null releaseNotes to be rejected")
	}
	if err := validateStrictJSON([]byte(`{"a":"\uD800"}`)); err == nil {
		t.Fatal("expected unpaired UTF-16 surrogate to be rejected")
	}
	if _, err := decodeCanonicalStdBase64("YWJj\n"); err == nil {
		t.Fatal("expected non-canonical base64 with newline to be rejected")
	}

	// P1-07：删除或 GC 后禁止同版本号复用。
	b2 := buildValidReleaseBundle(t, priv, "v4.0.1", "v0.9.0", testReleaseBaseURL, nil, nil)
	if _, err := store.publishStreaming(t.Context(), "beta", false, b2.manifestBytes, func() (io.Reader, error) {
		return bytes.NewReader(b2.archiveBytes), nil
	}, nil, "test"); err != nil {
		t.Fatalf("publish v4.0.1 failed: %v", err)
	}
	if err := store.deleteVersion("v4.0.1", "test"); err != nil {
		t.Fatalf("delete v4.0.1 failed: %v", err)
	}
	b2Reuse := buildValidReleaseBundle(t, priv, "v4.0.1", "v0.9.0", testReleaseBaseURL, map[string][]byte{
		"app/prompts/APPEND_SYSTEM.md": []byte("# modified content\n"),
	}, nil)
	if _, err := store.publishStreaming(t.Context(), "beta", true, b2Reuse.manifestBytes, func() (io.Reader, error) {
		return bytes.NewReader(b2Reuse.archiveBytes), nil
	}, nil, "test"); err == nil || !strings.Contains(err.Error(), "previously published") {
		t.Fatalf("expected version reuse to be rejected, got %v", err)
	}

	// P2-15：公共基址禁止空查询分隔符 ?。
	if err := validateReleasePublicBaseURL("https://www.cstoa.top/api/agent/releases?"); err == nil {
		t.Fatal("expected base URL with trailing ? to be rejected")
	}

	// P2-13：清单声明超过 MaxArchiveBytes 应返回 413 而非 422；多余 multipart 字段返回 400。
	srvSmall, _, _, privSmall := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.MaxArchiveBytes = 1024 // 1 KiB
	}, nil, nil)
	bBig := buildValidReleaseBundle(t, privSmall, "v4.1.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	rBig := httptest.NewRecorder()
	srvSmall.Handler.ServeHTTP(rBig, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, bBig.manifestBytes, bBig.archiveBytes, bBig.filename))
	if rBig.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rBig.Body.String(), `"archive_too_large"`) {
		t.Fatalf("oversize declared archive expected 413, got %d %s", rBig.Code, rBig.Body.String())
	}

	// P2-16：加密 PKCS#8 私钥头与大写 .JSON 凭据键拦截。
	srvSec, _, _, privSec := newTestReleaseEnv(t, nil, nil, nil)
	bEncKey := buildValidReleaseBundle(t, privSec, "v4.2.0", "v0.9.0", testReleaseBaseURL, map[string][]byte{
		"app/prompts/APPEND_SYSTEM.md": []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\nABC\n-----END ENCRYPTED PRIVATE KEY-----\n"),
	}, nil)
	rEnc := httptest.NewRecorder()
	srvSec.Handler.ServeHTTP(rEnc, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, bEncKey.manifestBytes, bEncKey.archiveBytes, bEncKey.filename))
	if rEnc.Code != http.StatusUnprocessableEntity {
		t.Fatalf("encrypted private key expected 422, got %d", rEnc.Code)
	}
	bUpperJSON := buildValidReleaseBundle(t, privSec, "v4.2.1", "v0.9.0", testReleaseBaseURL, map[string][]byte{
		"app/defaults/EXTRA.JSON": []byte(`{"token":"leaked"}`),
	}, nil)
	rUpper := httptest.NewRecorder()
	srvSec.Handler.ServeHTTP(rUpper, makeMultipartPublishRequest(t, "/v1/admin/releases", testReleaseAdminToken, bUpperJSON.manifestBytes, bUpperJSON.archiveBytes, bUpperJSON.filename))
	if rUpper.Code != http.StatusUnprocessableEntity {
		t.Fatalf("uppercase .JSON secret expected 422, got %d", rUpper.Code)
	}

	// P2-11：KeepCount 已满时 --no-activate 发布的新版本不应在同次事务被当场淘汰。
	srvKeep, storeKeep, _, privKeep := newTestReleaseEnv(t, func(c *releaseConfig) {
		c.KeepCount = 1
	}, nil, nil)
	bK1 := buildValidReleaseBundle(t, privKeep, "v5.0.0", "v0.9.0", testReleaseBaseURL, nil, nil)
	rK1 := httptest.NewRecorder()
	srvKeep.Handler.ServeHTTP(rK1, makeMultipartPublishRequest(t, "/v1/admin/releases?channel=stable&activate=true", testReleaseAdminToken, bK1.manifestBytes, bK1.archiveBytes, bK1.filename))
	if rK1.Code != http.StatusCreated {
		t.Fatalf("publish v5.0.0 failed: %d", rK1.Code)
	}
	bK2 := buildValidReleaseBundle(t, privKeep, "v5.0.1", "v0.9.0", testReleaseBaseURL, nil, nil)
	rK2 := httptest.NewRecorder()
	srvKeep.Handler.ServeHTTP(rK2, makeMultipartPublishRequest(t, "/v1/admin/releases?channel=stable&activate=false", testReleaseAdminToken, bK2.manifestBytes, bK2.archiveBytes, bK2.filename))
	if rK2.Code != http.StatusCreated {
		t.Fatalf("publish v5.0.1 (--no-activate) failed: %d", rK2.Code)
	}
	if _, ok := storeKeep.getFile("v5.0.1", bK2.filename); !ok {
		t.Fatal("newly published --no-activate version v5.0.1 was immediately evicted")
	}

	// 真实 HTTP/1.1 连接验证：合法发布正常返回 201，提前拒绝带 Connection: close。
	ts := httptest.NewServer(srvKeep.Handler)
	defer ts.Close()
	bLive := buildValidReleaseBundle(t, privKeep, "v5.0.2", "v0.9.0", testReleaseBaseURL, nil, nil)
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	p1, _ := mw.CreateFormFile("manifest", "m.json")
	_, _ = p1.Write(bLive.manifestBytes)
	p2, _ := mw.CreateFormFile("archive", bLive.filename)
	_, _ = p2.Write(bLive.archiveBytes)
	_ = mw.Close()
	reqLive, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/admin/releases?channel=stable&activate=true", &mp)
	reqLive.Header.Set("Authorization", "Bearer "+testReleaseAdminToken)
	reqLive.Header.Set("Content-Type", mw.FormDataContentType())
	respLive, err := ts.Client().Do(reqLive)
	if err != nil {
		t.Fatalf("live HTTP/1.1 publish failed: %v", err)
	}
	respLive.Body.Close()
	if respLive.StatusCode != http.StatusCreated {
		t.Fatalf("live HTTP/1.1 publish expected 201, got %d", respLive.StatusCode)
	}
}
