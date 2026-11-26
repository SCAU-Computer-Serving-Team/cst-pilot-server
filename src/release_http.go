// 发行包分发 · HTTP 公开下载与管理路由。
package main

import (
	"context"
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
	// 仅在无 If-Match 前置条件时允许基于 ETag 的 304 快速省流判断。
	if r.Header.Get("If-Match") == "" && etagMatches(r.Header.Get("If-None-Match"), etag) {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if r.Method == http.MethodHead {
		setWriteDeadline(w, 15*time.Second)
		setArchiveResponseHeaders(w, entry, etag)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(entry.ArchiveSize, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Nginx X-Accel-Redirect 模式：鉴权限流与流量记账后交由内部 location 零拷贝发送。
	if h.store.cfg.AccelPrefix != "" {
		clientIP := releaseClientIP(r, h.store.cfg.TrustProxy)
		decision, retryAfter := h.guard.allowFileDownload(clientIP, entry.ArchiveSize)
		if decision != downloadAllowed {
			setWriteDeadline(w, 15*time.Second)
			writeDownloadDenied(w, decision, retryAfter)
			return
		}
		setWriteDeadline(w, 15*time.Second)
		setArchiveResponseHeaders(w, entry, etag)
		// 内部 alias 指向 /var/lib/cst-releases/versions/，仅暴露版本目录。
		accelPath := h.store.cfg.AccelPrefix + "/" + entry.Version + "/" + entry.Filename
		w.Header().Set("X-Accel-Redirect", accelPath)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Go 原生直出模式：先占并发槽并打开验证文件，再扣流量预算，失败路径不消耗预算。
	if !h.guard.acquireDirectSlot() {
		setWriteDeadline(w, 15*time.Second)
		w.Header().Set("Retry-After", "5")
		sendJSON(w, http.StatusServiceUnavailable, errorBody("server_busy", "too many concurrent downloads"))
		return
	}
	defer h.guard.releaseDirectSlot()

	info, err := os.Lstat(entry.ArchivePath)
	if err != nil || !info.Mode().IsRegular() {
		setWriteDeadline(w, 15*time.Second)
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive unavailable"))
		return
	}
	file, err := os.Open(entry.ArchivePath)
	if err != nil {
		setWriteDeadline(w, 15*time.Second)
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive unavailable"))
		return
	}
	defer file.Close()
	if fInfo, err := file.Stat(); err != nil || !fInfo.Mode().IsRegular() || fInfo.Size() != entry.ArchiveSize {
		setWriteDeadline(w, 15*time.Second)
		sendJSON(w, http.StatusNotFound, errorBody("release_not_found", "release archive unavailable"))
		return
	}

	clientIP := releaseClientIP(r, h.store.cfg.TrustProxy)
	decision, retryAfter := h.guard.allowFileDownload(clientIP, entry.ArchiveSize)
	if decision != downloadAllowed {
		setWriteDeadline(w, 15*time.Second)
		writeDownloadDenied(w, decision, retryAfter)
		return
	}

	setArchiveResponseHeaders(w, entry, etag)
	modTime := info.ModTime()
	if t, err := time.Parse(time.RFC3339Nano, entry.PublishedAt); err == nil {
		modTime = t
	}
	// 放宽连接写截止至大包总下载超时上限，保留底层 *os.File -> net.Conn 的 Linux sendfile 零拷贝能力。
	setWriteDeadline(w, releaseDownloadTotalTimeout)
	http.ServeContent(w, r, entry.Filename, modTime, file)
}

func setArchiveResponseHeaders(w http.ResponseWriter, entry *fileSnapshot, etag string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.Filename))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeDownloadDenied(w http.ResponseWriter, decision downloadDecision, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	if decision == downloadDenyRateLimited {
		sendJSON(w, http.StatusTooManyRequests, errorBody("rate_limited", "too many download requests"))
	} else {
		sendJSON(w, http.StatusServiceUnavailable, errorBody("bandwidth_budget_exceeded", "release download budget temporarily exhausted"))
	}
}

func (h *releaseHandlers) authorizeAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	ok, disabled, limited, retryAfter := h.guard.checkAdminToken(r)
	if ok {
		return "admin-http:" + releaseClientIP(r, h.store.cfg.TrustProxy), true
	}
	setWriteDeadline(w, 15*time.Second)
	if r.Method == http.MethodPost {
		w.Header().Set("Connection", "close")
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
		// 鉴权前先施加短读超时防止未鉴权慢连接占坑；鉴权后再进入上传看门狗。
		setRouteDeadlines(w, 10*time.Second, 30*time.Second)
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
		obj, err := parseStrictJSONObject(body)
		if err != nil {
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_json", err.Error()))
			return
		}
		if err := requireExactKeys(obj, "version"); err != nil {
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_json", err.Error()))
			return
		}
		version, err := requireJSONString(obj, "version")
		if err != nil || version == "" {
			sendJSON(w, http.StatusBadRequest, errorBody("invalid_json", "version is required"))
			return
		}
		rec, err := h.store.activateChannel(channel, version, actor)
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
	abortPublish := func(status int, code, msg string) {
		// 提前拒绝时关闭 HTTP/1.1 连接且不排空大请求体，立即释放内存与连接。
		w.Header().Set("Connection", "close")
		setWriteDeadline(w, 15*time.Second)
		sendJSON(w, status, errorBody(code, msg))
	}

	q := r.URL.Query()
	channel := q.Get("channel")
	if channel == "" {
		channel = "stable"
	}
	if !isValidReleaseChannel(channel) {
		abortPublish(http.StatusBadRequest, "invalid_channel", "channel must be stable or beta")
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
			abortPublish(http.StatusBadRequest, "invalid_parameter", "activate must be true or false")
			return
		}
	}

	// 在读取任何 multipart 数据前先拿独占发布槽，防止并发请求各自读 8 MiB 清单挤占 48 MiB 堆内存。
	releaseSlot, err := h.store.tryAcquireWriteSlot()
	if err != nil {
		abortPublish(http.StatusConflict, "publish_in_progress", err.Error())
		return
	}
	defer releaseSlot()

	// 上传与校验阶段移除提前设定的 30 秒写截止，等准备发送最终响应时再设新鲜写截止，避免慢上传后发 201 超时。
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	ctx, cancel := context.WithTimeout(r.Context(), releaseUploadTotalTimeout)
	defer cancel()

	// 叠加请求体总上限（manifest + archive + multipart 封装余量）与滑动读超时。
	maxBodyBytes := int64(maxManifestBytes) + h.store.cfg.MaxArchiveBytes + (1 << 20)
	readWatchdog := &slidingDeadlineReader{
		r:            io.LimitReader(r.Body, maxBodyBytes+1),
		rc:           rc,
		hardDeadline: time.Now().Add(releaseUploadTotalTimeout),
		step:         releaseUploadStepTimeout,
	}
	readWatchdog.bump()
	r.Body = io.NopCloser(readWatchdog)

	mr, err := r.MultipartReader()
	if err != nil {
		abortPublish(http.StatusBadRequest, "invalid_multipart", "request must be multipart/form-data")
		return
	}

	// 第 1 段必须为 manifest，拒绝路径不调用会排空整段的 Part.Close()。
	firstPart, err := mr.NextPart()
	if err != nil {
		abortPublish(http.StatusBadRequest, "invalid_multipart", "missing manifest part")
		return
	}
	if firstPart.FormName() != "manifest" {
		abortPublish(http.StatusBadRequest, "invalid_multipart", "first multipart field must be manifest")
		return
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(firstPart, maxManifestBytes+1))
	if err != nil {
		abortPublish(http.StatusBadRequest, "invalid_body", "failed reading manifest part")
		return
	}
	if len(manifestBytes) > maxManifestBytes {
		abortPublish(http.StatusRequestEntityTooLarge, "manifest_too_large", "manifest exceeds 8 MiB limit")
		return
	}

	nextArchive := func() (io.Reader, error) {
		part, err := mr.NextPart()
		if err != nil {
			return nil, wrapReleaseErr(errInvalidMultipart, "missing archive part after manifest")
		}
		if part.FormName() != "archive" {
			return nil, wrapReleaseErr(errInvalidMultipart, "second multipart field must be archive")
		}
		return part, nil
	}
	afterArchive := func() error {
		extra, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return wrapReleaseErr(errInvalidMultipart, "malformed multipart boundary after archive")
		}
		return wrapReleaseErr(errInvalidMultipart, "unexpected extra multipart field %q", extra.FormName())
	}

	res, err := h.store.publishStreamingLocked(ctx, channel, activate, manifestBytes, nextArchive, afterArchive, actor)
	if err != nil {
		w.Header().Set("Connection", "close")
		setWriteDeadline(w, 15*time.Second)
		sendReleaseError(w, err)
		return
	}
	setWriteDeadline(w, 30*time.Second)
	sendJSON(w, http.StatusCreated, res)
}

var errInvalidMultipart = errors.New("invalid_multipart")

func sendReleaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		sendJSON(w, http.StatusRequestTimeout, errorBody("request_canceled", "upload canceled by client"))
	case errors.Is(err, context.DeadlineExceeded):
		sendJSON(w, http.StatusRequestTimeout, errorBody("upload_timeout", "upload processing exceeded deadline"))
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
