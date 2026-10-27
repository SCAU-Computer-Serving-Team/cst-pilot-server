// 遥测接收端 · HTTP 服务。
//
// POST /v1/sessions 写统计与心跳，GET 同路径声明能力，GET /healthz 检查健康。
// 不开放记录读取或命令下发接口。
// 单批上限：50 条或 1 MB，超出整批拒绝不截断（截断会让发送端把没送到的记录也删掉）。
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type serverOptions struct {
	store   *store
	auth    *introspector
	limiter *anonymousLimiter
	now     func() time.Time
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
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.limiter == nil {
		opts.limiter = &anonymousLimiter{}
	}
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
		sendJSON(w, 200, map[string]any{"ok": true, "sessions": sessions, "version": buildVersion, "builtAt": buildTime})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			sendJSON(w, 200, map[string]any{"protocols": []string{"0.1", "0.2"}, "capabilities": []string{"session-snapshots", "revision-acks", "heartbeat"}})
			return
		}
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

	installation := ""
	uploadIdentity := introspectResult{active: true}
	if token == "" {
		installation = strings.TrimPrefix(r.Header.Get("Authorization"), "Installation ")
		if !installationPattern.MatchString(installation) {
			fail(401, "unauthorized", "missing upload credential")
			return
		}
		if !opts.limiter.allow(ip, installation) {
			sendJSON(w, 429, errorBody("rate_limited", "retry later"))
			return
		}
	} else {
		uploadIdentity = opts.auth.introspect(token)
	}
	if uploadIdentity.err {
		fail(503, "introspection_unavailable", "identity service down")
		return
	}
	if !uploadIdentity.active {
		code := "unauthorized"
		status := 401
		if uploadIdentity.reason == "revoked" || uploadIdentity.reason == "password-changed" {
			code = "forbidden"
			status = 403
		}
		fail(status, code, uploadIdentity.reason)
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
	if token != "" {
		installation = r.Header.Get("X-Telemetry-Installation")
	}
	var publicKey string
	if installation != "" {
		var verified bool
		publicKey, verified = verifyInstallation(r, body, installation)
		if !verified {
			fail(401, "invalid_signature", "invalid installation signature")
			return
		}
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

	snapshots := str(parsed["v"]) == "0.2"
	if snapshots && installation == "" {
		fail(401, "installation_required", "snapshot uploads require installation proof")
		return
	}
	if installation != "" {
		registered, err := opts.store.registerInstallation(installation, publicKey)
		if err != nil {
			fail(503, "registration_unavailable", "installation registration failed")
			return
		}
		if !registered {
			fail(403, "installation_key_mismatch", "installation key already bound")
			return
		}
	}

	type acceptedRecord struct {
		recordID string
		payload  map[string]any
	}
	var accepted []acceptedRecord
	acks := []snapshotAck{}
	var rejectedDetail []map[string]any
	reasons := map[string]int{}
	for index, record := range records {
		reason := validateRecord(record)
		if snapshots && str(record["v"]) != "0.2" {
			reason = "envelope-version-mismatch"
		}
		if !snapshots && str(record["v"]) == "0.2" {
			reason = "envelope-version-mismatch"
		}
		if installation != "" {
			claimed, _ := record["identity"].(map[string]any)
			if id := str(claimed["installationId"]); (snapshots || id != "") && id != installation {
				reason = "installation-mismatch"
			}
		}
		if reason == "" {
			accepted = append(accepted, acceptedRecord{recordID: str(record["recordId"]), payload: record})
			continue
		}
		if snapshots {
			acks = append(acks, snapshotAck{RecordID: str(record["recordId"]), Revision: num(record["revision"]), Status: "rejected", Reason: reason})
		}
		rejectedDetail = append(rejectedDetail, map[string]any{"index": index, "recordId": str(record["recordId"]), "reason": reason})
		reasons[reason]++
	}

	receivedAt := utcNow(opts.now())
	var inserted int
	if len(accepted) > 0 {
		rows := make([]insertRow, 0, len(accepted))
		for _, entry := range accepted {
			rows = append(rows, insertRow{
				recordID: entry.recordID, mid: uploadIdentity.identity.MID, deviceID: uploadIdentity.identity.DeviceID,
				receivedAt: receivedAt, ipNet: ipNet, payload: entry.payload,
			})
		}
		var n int
		var err error
		if snapshots {
			var stored []snapshotAck
			stored, n, err = opts.store.applySnapshots(rows, installation)
			acks = append(acks, stored...)
			for _, ack := range stored {
				if ack.Status == "rejected" {
					rejectedDetail = append(rejectedDetail, map[string]any{"recordId": ack.RecordID, "revision": ack.Revision, "reason": ack.Reason})
					reasons[ack.Reason]++
				}
			}
		} else {
			n, err = opts.store.insertBatch(rows)
		}
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
		batchID: batchID, receivedAt: receivedAt, mid: uploadIdentity.identity.MID, deviceID: uploadIdentity.identity.DeviceID,
		recordCount: len(records), accepted: len(records) - len(rejectedDetail), rejected: len(rejectedDetail), inserted: inserted,
		rejectedDetail: rejectedDetail, statusCode: 202, bodyBytes: len(body),
		durationMs: time.Since(startedAt).Milliseconds(), ipNet: ipNet,
	})
	result := map[string]any{"accepted": len(records) - len(rejectedDetail), "rejected": len(rejectedDetail), "reasons": reasons}
	if snapshots {
		result["v"], result["acks"] = "0.2", acks
		if heartbeat, exists := parsed["heartbeat"]; exists {
			ack, err := opts.store.applyHeartbeat(heartbeat, installation, opts.now())
			if err != nil {
				fail(500, "server_error", err.Error())
				return
			}
			result["heartbeatAck"] = ack
		}
	}
	sendJSON(w, 202, result)
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
