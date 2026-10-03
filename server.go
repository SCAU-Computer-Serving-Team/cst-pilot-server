// 遥测接收端 · HTTP 服务。
//
// 一个写端点 POST /v1/sessions，一个 GET /healthz；不开对外读接口。
// 单批上限：50 条或 1 MB，超出整批拒绝不截断（截断会让发送端把没送到的记录也删掉）。
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"
)

type serverOptions struct {
	store *store
	auth  *introspector
}

// ipNetOf IPv4 记 /24 网段，其余原样。
func ipNetOf(ip string) string {
	parsed := net.ParseIP(ip)
	if v4 := parsed.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip
}

func sendJSON(w http.ResponseWriter, status int, body any) {
	payload, err := jsonMarshal(body)
	if err != nil {
		payload = []byte(`{"error":{"code":"server_error","message":"encode failed"}}`)
		status = 500
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(payload)))
	w.WriteHeader(status)
	w.Write(payload)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func newTelemetryServer(opts serverOptions) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sendJSON(w, 405, errorBody("method_not_allowed", "use GET"))
			return
		}
		sessions, err := opts.store.countSessions()
		if err != nil {
			sendJSON(w, 500, errorBody("server_error", err.Error()))
			return
		}
		sendJSON(w, 200, map[string]any{"ok": true, "sessions": sessions})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			sendJSON(w, 405, errorBody("method_not_allowed", "use POST"))
			return
		}
		handleUpload(w, r, opts)
	})
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

type errorEnvelope struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errorBody(code, message string) errorEnvelope {
	return errorEnvelope{Error: errorInfo{Code: code, Message: message}}
}

func handleUpload(w http.ResponseWriter, r *http.Request, opts serverOptions) {
	startedAt := time.Now()
	ip := ""
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	ipNet := ipNetOf(ip)
	token := bearerToken(r.Header.Get("Authorization"))

	fail := func(status int, code, message string) {
		opts.store.logUpload(uploadLog{
			receivedAt: utcNow(time.Now()), statusCode: status, errCode: code,
			durationMs: time.Since(startedAt).Milliseconds(), ipNet: ipNet,
		})
		sendJSON(w, status, errorBody(code, message))
	}

	if token == "" {
		fail(401, "unauthorized", "missing bearer token")
		return
	}
	identity := opts.auth.introspect(token)
	if identity.err {
		fail(503, "introspection_unavailable", "identity service down")
		return
	}
	if !identity.active {
		code := "unauthorized"
		status := 401
		if identity.reason == "revoked" || identity.reason == "password-changed" {
			code = "forbidden"
			status = 403
		}
		fail(status, code, identity.reason)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, envelopeMaxBytes+1))
	if err != nil {
		fail(400, "invalid_body", "cannot read body")
		return
	}
	if len(body) > envelopeMaxBytes {
		fail(413, "payload_too_large", "batch exceeds 1 MB")
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		fail(400, "invalid_json", "body is not valid JSON")
		return
	}
	records, status, code := validateEnvelope(body)
	if records == nil {
		fail(status, code, "envelope rejected")
		return
	}

	type acceptedRecord struct {
		recordID string
		payload  map[string]any
	}
	var accepted []acceptedRecord
	var rejectedDetail []map[string]any
	reasons := map[string]int{}
	for index, record := range records {
		reason := validateRecord(record)
		if reason == "" {
			accepted = append(accepted, acceptedRecord{recordID: str(record["recordId"]), payload: record})
			continue
		}
		rejectedDetail = append(rejectedDetail, map[string]any{"index": index, "recordId": str(record["recordId"]), "reason": reason})
		reasons[reason]++
	}

	receivedAt := utcNow(time.Now())
	var inserted int
	if len(accepted) > 0 {
		rows := make([]insertRow, 0, len(accepted))
		for _, entry := range accepted {
			rows = append(rows, insertRow{
				recordID: entry.recordID, mid: identity.identity.MID, deviceID: identity.identity.DeviceID,
				receivedAt: receivedAt, ipNet: ipNet, payload: entry.payload,
			})
		}
		n, err := opts.store.insertBatch(rows)
		if err != nil {
			fail(500, "server_error", err.Error())
			return
		}
		inserted = n
	}

	batchID := ""
	if b, ok := parsed["batchId"].(string); ok {
		batchID = b
	}
	if rejectedDetail == nil {
		rejectedDetail = []map[string]any{}
	}
	opts.store.logUpload(uploadLog{
		batchID: batchID, receivedAt: receivedAt, mid: identity.identity.MID, deviceID: identity.identity.DeviceID,
		recordCount: len(records), accepted: len(accepted), rejected: len(rejectedDetail), inserted: inserted,
		rejectedDetail: rejectedDetail, statusCode: 202, bodyBytes: len(body),
		durationMs: time.Since(startedAt).Milliseconds(), ipNet: ipNet,
	})
	sendJSON(w, 202, map[string]any{"accepted": len(accepted), "rejected": len(rejectedDetail), "reasons": reasons})
}

func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !equalFold(header[:len(prefix)], prefix) {
		return ""
	}
	return header[len(prefix):]
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
