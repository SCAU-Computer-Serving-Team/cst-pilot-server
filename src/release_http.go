// 发行包分发 · HTTP 公开下载与管理路由。
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	releaseUploadTotalTimeout   = 10 * time.Minute
	releaseUploadStepTimeout    = 30 * time.Second
	releaseDownloadTotalTimeout = 10 * time.Minute
	releaseDownloadStepTimeout  = 30 * time.Second
)

type releaseHandlers struct {
	store *releaseStore
	guard *releaseGuard
}

func registerReleaseRoutes(mux *http.ServeMux, store *releaseStore, guard *releaseGuard) {
	if store == nil || guard == nil {
		return
	}
	h := &releaseHandlers{store: store, guard: guard}
	mux.HandleFunc("/v1/releases/channels/", h.handleChannelLatest)
	mux.HandleFunc("/v1/releases/files/", h.handleArchiveFile)
	mux.HandleFunc("/v1/admin/releases", h.handleAdminRoot)
	mux.HandleFunc("/v1/admin/releases/", h.handleAdminSubpath)
}

func (h *releaseHandlers) handleChannelLatest(w http.ResponseWriter, r *http.Request) {
	setRouteDeadlines(w, 10*time.Second, 15*time.Second)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		sendJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "use GET or HEAD"))
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/v1/releases/channels/")
	parts := strings.Split(rel, "/")
	if len(parts) != 2 || parts[1] != "latest" || !isValidReleaseChannel(parts[0]) {
		sendJSON(w, http.StatusNotFound, errorBody("invalid_channel", "unknown release channel"))
		return
	}
	ch, ok := h.store.getChannel(parts[0])
	if !ok {
		sendJSON(w, http.StatusNotFound, errorBody("channel_empty", "no active release published on this channel"))
		return
	}
	etag := `"` + ch.ManifestSHA256 + `"`
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(ch.ManifestBytes)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(ch.ManifestBytes)
	}
}

func (h *releaseHandlers) handleArchiveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setRouteDeadlines(w, 10*time.Second, 15*time.Second)
		sendJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "use GET or HEAD"))
		return
	}
	setReadDeadline(w, 10*time.Second)

	rel := strings.TrimPrefix(r.URL.Path, "/v1/releases/files/")
	parts := strings.Split(rel, "/")
	if len(parts) != 2 || !isValidReleaseVersionString(parts[0]) || parts[1] == "" {
		setWriteDeadline(w, 15*time.Second)
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive not found"))
		return
	}
	entry, ok := h.store.getFile(parts[0], parts[1])
	if !ok {
		setWriteDeadline(w, 15*time.Second)
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive not found"))
		return
	}

	etag := `"` + entry.ArchiveSHA256 + `"`
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.Filename))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		setWriteDeadline(w, 15*time.Second)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(entry.ArchiveSize, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	clientIP := releaseClientIP(r, h.store.cfg.TrustProxy)
	decision, retryAfter := h.guard.allowFileDownload(clientIP, entry.ArchiveSize)
	if decision != downloadAllowed {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Del("Content-Disposition")
		w.Header().Del("ETag")
		w.Header().Del("Cache-Control")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		if decision == downloadDenyRateLimited {
			sendJSON(w, http.StatusTooManyRequests, errorBody("rate_limited", "too many download requests"))
		} else {
			sendJSON(w, http.StatusServiceUnavailable, errorBody("bandwidth_budget_exceeded", "release download budget temporarily exhausted"))
		}
		return
	}

	if h.store.cfg.AccelPrefix != "" {
		setWriteDeadline(w, 15*time.Second)
		accelPath := h.store.cfg.AccelPrefix + "/" + releaseVersionsDir + "/" + entry.Version + "/" + entry.Filename
		w.Header().Set("X-Accel-Redirect", accelPath)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Go 原生直出模式（本地/测试/无 Nginx）：信号量隔离 + 滑动写超时 + sendfile ServeContent。
	if !h.guard.acquireDirectSlot() {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Del("Content-Disposition")
		w.Header().Del("ETag")
		w.Header().Del("Cache-Control")
		w.Header().Set("Retry-After", "5")
		sendJSON(w, http.StatusServiceUnavailable, errorBody("server_busy", "too many concurrent downloads"))
		return
	}
	defer h.guard.releaseDirectSlot()

	info, err := os.Lstat(entry.ArchivePath)
	if err != nil || !info.Mode().IsRegular() {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Del("Content-Disposition")
		w.Header().Del("ETag")
		w.Header().Del("Cache-Control")
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive unavailable"))
		return
	}
	file, err := os.Open(entry.ArchivePath)
	if err != nil {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Del("Content-Disposition")
		w.Header().Del("ETag")
		w.Header().Del("Cache-Control")
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive unavailable"))
		return
	}
	defer file.Close()

	modTime := info.ModTime()
	if t, err := time.Parse(time.RFC3339Nano, entry.PublishedAt); err == nil {
		modTime = t
	}
	wrapped := &slidingDeadlineResponseWriter{
		ResponseWriter: w,
		rc:             http.NewResponseController(w),
		hardDeadline:   time.Now().Add(releaseDownloadTotalTimeout),
		step:           releaseDownloadStepTimeout,
	}
	wrapped.bump()
	http.ServeContent(wrapped, r, entry.Filename, modTime, file)
}

func (h *releaseHandlers) authorizeAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	ok, disabled, limited, retryAfter := h.guard.checkAdminToken(r)
	if ok {
		return "admin-http:" + releaseClientIP(r, h.store.cfg.TrustProxy), true
	}
	if disabled {
		sendJSON(w, http.StatusServiceUnavailable, errorBody("admin_disabled", "release admin token is not configured"))
		return "", false
	}
	if limited {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		sendJSON(w, http.StatusTooManyRequests, errorBody("rate_limited", "too many failed authentication attempts"))
		return "", false
	}
	sendJSON(w, http.StatusUnauthorized, errorBody("unauthorized", "invalid admin token"))
	return "", false
}

func (h *releaseHandlers) handleAdminRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		setRouteDeadlines(w, 15*time.Second, 30*time.Second)
		if _, ok := h.authorizeAdmin(w, r); !ok {
			return
		}
		resp := h.store.inventorySnapshot()
		resp["bandwidth"] = h.guard.budgetStatus()
		sendJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		setWriteDeadline(w, 30*time.Second)
		actor, ok := h.authorizeAdmin(w, r)
		if !ok {
			return
		}
		h.handleAdminPublish(w, r, actor)
	default:
		setRouteDeadlines(w, 15*time.Second, 30*time.Second)
		sendJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "use GET or POST"))
	}
}

func (h *releaseHandlers) handleAdminSubpath(w http.ResponseWriter, r *http.Request) {
	setRouteDeadlines(w, 15*time.Second, 30*time.Second)
	actor, ok := h.authorizeAdmin(w, r)
	if !ok {
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, "/v1/admin/releases/")
	switch {
	case sub == "":
		h.handleAdminRoot(w, r)
	case sub == "gc":
		if r.Method != http.MethodPost {
			sendJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "use POST"))
			return
		}
		evicted, err := h.store.runGC(actor)
		if err != nil {
			sendReleaseError(w, err)
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"evicted": evicted})
	case strings.HasPrefix(sub, "channels/"):
		if r.Method != http.MethodPut {
			sendJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "use PUT"))
			return
		}
		channel := strings.TrimPrefix(sub, "channels/")
		if strings.Contains(channel, "/") || !isValidReleaseChannel(channel) {
			sendJSON(w, http.StatusNotFound, errorBody("invalid_channel", "unknown release channel"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096+1))
		if err != nil || len(body) > 4096 {
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_body", "cannot read request body"))
			return
		}
		if err := validateStrictJSON(body); err != nil {
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_json", err.Error()))
			return
		}
		var req struct {
			Version string `json:"version"`
		}
		if err := decodeStrictJSONStruct(body, &req); err != nil || req.Version == "" {
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_json", "version is required"))
			return
		}
		rec, err := h.store.activateChannel(channel, req.Version, actor)
		if err != nil {
			sendReleaseError(w, err)
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"channel": channel, "record": rec})
	case strings.HasPrefix(sub, "versions/"):
		if r.Method != http.MethodDelete {
			sendJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "use DELETE"))
			return
		}
		ver := strings.TrimPrefix(sub, "versions/")
		if strings.Contains(ver, "/") || !isValidReleaseVersionString(ver) {
			sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "invalid version"))
			return
		}
		if err := h.store.deleteVersion(ver, actor); err != nil {
			sendReleaseError(w, err)
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"deleted": ver})
	default:
		sendJSON(w, http.StatusNotFound, errorBody("not_found", "unknown admin endpoint"))
	}
}

func (h *releaseHandlers) handleAdminPublish(w http.ResponseWriter, r *http.Request, actor string) {
	q := r.URL.Query()
	channel := q.Get("channel")
	if channel == "" {
		channel = "stable"
	}
	if !isValidReleaseChannel(channel) {
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_channel", "channel must be stable or beta"))
		return
	}
	activate := true
	if rawAct := q.Get("activate"); rawAct != "" {
		switch strings.ToLower(rawAct) {
		case "true", "1":
			activate = true
		case "false", "0":
			activate = false
		default:
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_parameter", "activate must be true or false"))
			return
		}
	}

	// 流式读看门狗：总上传限时 10m，每次读进展续期 30s。
	rc := http.NewResponseController(w)
	readWatchdog := &slidingDeadlineReader{
		r:            r.Body,
		rc:           rc,
		hardDeadline: time.Now().Add(releaseUploadTotalTimeout),
		step:         releaseUploadStepTimeout,
	}
	readWatchdog.bump()
	r.Body = io.NopCloser(readWatchdog)

	mr, err := r.MultipartReader()
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_multipart", "request must be multipart/form-data"))
		return
	}

	// 第 1 段必须为 manifest，先读出验签查重，拒绝时不落盘 ZIP。
	firstPart, err := mr.NextPart()
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_multipart", "missing manifest part"))
		return
	}
	if firstPart.FormName() != "manifest" {
		_ = firstPart.Close()
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_multipart", "first multipart field must be manifest"))
		return
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(firstPart, maxManifestBytes+1))
	_ = firstPart.Close()
	if err != nil {
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_body", "failed reading manifest part"))
		return
	}
	if len(manifestBytes) > maxManifestBytes {
		sendJSON(w, http.StatusRequestEntityTooLarge, errorBody("manifest_too_large", "manifest exceeds 8 MiB limit"))
		return
	}

	var archivePart io.ReadCloser
	defer func() {
		if archivePart != nil {
			_ = archivePart.Close()
		}
	}()
	nextArchive := func() (io.Reader, error) {
		part, err := mr.NextPart()
		if err != nil {
			return nil, wrapReleaseErr(errInvalidMultipart, "missing archive part after manifest")
		}
		if part.FormName() != "archive" {
			_ = part.Close()
			return nil, wrapReleaseErr(errInvalidMultipart, "second multipart field must be archive")
		}
		archivePart = part
		return part, nil
	}

	res, err := h.store.publishStreaming(channel, activate, manifestBytes, nextArchive, actor)
	if err != nil {
		sendReleaseError(w, err)
		return
	}
	sendJSON(w, http.StatusCreated, res)
}

var errInvalidMultipart = errors.New("invalid_multipart")

func sendReleaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidChannel):
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_channel", err.Error()))
	case errors.Is(err, errInvalidMultipart):
		sendJSON(w, http.StatusBadRequest, errorBody("invalid_multipart", err.Error()))
	case errors.Is(err, errReleaseBusy):
		sendJSON(w, http.StatusConflict, errorBody("publish_in_progress", err.Error()))
	case errors.Is(err, errVersionExists):
		sendJSON(w, http.StatusConflict, errorBody("version_already_exists", err.Error()))
	case errors.Is(err, errVersionActive):
		sendJSON(w, http.StatusConflict, errorBody("version_active", err.Error()))
	case errors.Is(err, errVersionNotFound):
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", err.Error()))
	case errors.Is(err, errArchiveTooLarge):
		sendJSON(w, http.StatusRequestEntityTooLarge, errorBody("archive_too_large", err.Error()))
	case errors.Is(err, errVerifyFailed):
		sendJSON(w, http.StatusUnprocessableEntity, errorBody("verification_failed", err.Error()))
	case errors.Is(err, errInsufficientStorage):
		sendJSON(w, http.StatusInsufficientStorage, errorBody("insufficient_storage", err.Error()))
	default:
		sendJSON(w, http.StatusInternalServerError, errorBody("server_error", err.Error()))
	}
}

func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	for _, token := range strings.Split(ifNoneMatch, ",") {
		t := strings.TrimSpace(token)
		if t == "*" || t == etag || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

func setRouteDeadlines(w http.ResponseWriter, readDur, writeDur time.Duration) {
	setReadDeadline(w, readDur)
	setWriteDeadline(w, writeDur)
}

func setReadDeadline(w http.ResponseWriter, dur time.Duration) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(dur))
}

func setWriteDeadline(w http.ResponseWriter, dur time.Duration) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(dur))
}

type slidingDeadlineReader struct {
	r            io.Reader
	rc           *http.ResponseController
	hardDeadline time.Time
	step         time.Duration
}

func (s *slidingDeadlineReader) bump() {
	next := time.Now().Add(s.step)
	if next.After(s.hardDeadline) {
		next = s.hardDeadline
	}
	_ = s.rc.SetReadDeadline(next)
}

func (s *slidingDeadlineReader) Read(p []byte) (int, error) {
	s.bump()
	return s.r.Read(p)
}

type slidingDeadlineResponseWriter struct {
	http.ResponseWriter
	rc           *http.ResponseController
	hardDeadline time.Time
	step         time.Duration
}

func (s *slidingDeadlineResponseWriter) bump() {
	next := time.Now().Add(s.step)
	if next.After(s.hardDeadline) {
		next = s.hardDeadline
	}
	_ = s.rc.SetWriteDeadline(next)
}

func (s *slidingDeadlineResponseWriter) Write(p []byte) (int, error) {
	s.bump()
	return s.ResponseWriter.Write(p)
}

func (s *slidingDeadlineResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	s.bump()
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(&slidingCopyReader{r: r, bump: s.bump})
	}
	return io.Copy(s.ResponseWriter, &slidingCopyReader{r: r, bump: s.bump})
}

type slidingCopyReader struct {
	r    io.Reader
	bump func()
}

func (s *slidingCopyReader) Read(p []byte) (int, error) {
	s.bump()
	return s.r.Read(p)
}
