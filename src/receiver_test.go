package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.close() })
	return st
}

func signedUpload(t *testing.T, server *http.Server, private ed25519.PrivateKey, installation, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(body))
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	digest := sha256.Sum256([]byte(body))
	message := strings.Join([]string{"telemetry-upload", installation, stamp, hex.EncodeToString(digest[:])}, "\n")
	publicDER, err := x509.MarshalPKIXPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Installation "+installation)
	req.Header.Set("X-Telemetry-Key", base64.RawURLEncoding.EncodeToString(publicDER))
	req.Header.Set("X-Telemetry-Time", stamp)
	req.Header.Set("X-Telemetry-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(message))))
	for _, change := range mutate {
		change(req)
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, req)
	return response
}

func TestAnonymousUploadPersistsPortableEnvironment(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	installation := "df79039e-3744-44c8-9754-7a9568ae5684"
	body := `{"v":"0.1","records":[{"v":"0.1","type":"session","recordId":"anonymous-env","mid":"spoof","deviceId":"spoof","identity":{"installationId":"` + installation + `","environmentId":"env-a","environmentSource":"windows-machine-guid"},"os":{"platform":"win32","name":"Windows 11 Pro","displayVersion":"24H2","version":"10.0.26100","arch":"arm64","isPE":false},"runtime":{"version":"24.15.0","arch":"x64"},"admin":null}]}`
	for range 2 {
		response := signedUpload(t, server, key, installation, body)
		if response.Code != 202 {
			t.Fatalf("anonymous upload = %d %s", response.Code, response.Body)
		}
	}
	if count, _ := st.countSessions(); count != 1 {
		t.Fatalf("dedup count = %d", count)
	}
	csv, err := st.exportSessionsCsv()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{installation, "env-a", "Windows 11 Pro", "24H2", "24.15.0"} {
		if !strings.Contains(csv, expected) {
			t.Fatalf("CSV missing %s: %s", expected, csv)
		}
	}
	if strings.Contains(csv, "spoof") {
		t.Fatal("client identity trusted")
	}
}

func TestInstallationCredentialBoundaries(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	installation := "b1f77e2a-4efb-41fc-9ff4-e4a116b68959"
	body := `{"records":[{"v":"0.1","type":"session","recordId":"signed-record"}]}`
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"signature", func(r *http.Request) { r.Header.Set("X-Telemetry-Signature", "invalid") }},
		{"clock", func(r *http.Request) { r.Header.Set("X-Telemetry-Time", "1") }},
		{"body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(body, "signed-record", "changed")))
		}},
		{"public-key", func(r *http.Request) { r.Header.Set("X-Telemetry-Key", "invalid") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if response := signedUpload(t, server, key, installation, body, tc.change); response.Code != 401 {
				t.Fatalf("tampered upload = %d %s", response.Code, response.Body)
			}
		})
	}
	var registrations int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM installations").Scan(&registrations); err != nil || registrations != 0 {
		t.Fatalf("invalid signature registered key: %d %v", registrations, err)
	}
	if response := signedUpload(t, server, key, installation, body); response.Code != 202 {
		t.Fatalf("valid signature = %d", response.Code)
	}
	// 重建服务实例模拟进程重启，公钥绑定来自数据库。
	server = newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	if response := signedUpload(t, server, wrong, installation, body); response.Code != 403 {
		t.Fatalf("changed key = %d", response.Code)
	}
	if response := signedUpload(t, server, key, installation, body); response.Code != 202 {
		t.Fatalf("original key = %d", response.Code)
	}
	other := strings.ReplaceAll(body, `"recordId":"signed-record"`, `"recordId":"wrong-installation","identity":{"installationId":"other"}`)
	response := signedUpload(t, server, key, installation, other)
	if response.Code != 202 || !strings.Contains(response.Body.String(), `"installation-mismatch":1`) {
		t.Fatalf("installation spoof accepted: %d %s", response.Code, response.Body)
	}
	if count, _ := st.countSessions(); count != 1 {
		t.Fatalf("invalid uploads inserted: %d", count)
	}
}

func TestAnonymousUploadRateLimit(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	for index := 0; index <= 60; index++ {
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Installation b1f77e2a-4efb-41fc-9ff4-e4a116b68959")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, req)
		if index < 60 && response.Code != 401 {
			t.Fatalf("invalid signature returned %d", response.Code)
		}
		if index == 60 && response.Code != 429 {
			t.Fatalf("rate limit returned %d", response.Code)
		}
	}
}

func TestExistingDatabaseMigrationAndAnonymousRollup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(schema, "\n\tinstallation_id TEXT NOT NULL DEFAULT '',\n\tenvironment_id TEXT NOT NULL DEFAULT '',", "", 1)
	oldSchema = strings.Replace(oldSchema, "PRIMARY KEY (date, mid, installation_id, environment_id)", "PRIMARY KEY (date, mid)", 1)
	if _, err = db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO sessions (record_id,mid,device_id,received_at,payload) VALUES ('legacy','member','device',?,'{"v":"0.1","type":"session","recordId":"legacy","os":{"arch":"x64"}}')`, utcNow(time.Now())); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"installation-a", "installation-b"} {
		_, err := st.insertBatch([]insertRow{{recordID: id, receivedAt: utcNow(time.Now()), payload: map[string]any{"identity": map[string]any{"installationId": id, "environmentId": "environment"}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.refreshRollup(); err != nil {
		t.Fatal(err)
	}
	var groups int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM daily_rollup").Scan(&groups); err != nil || groups != 3 {
		t.Fatalf("anonymous origins merged: %d %v", groups, err)
	}
	st.close()
	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	csv, err := st.exportSessionsCsv()
	if err != nil || !strings.Contains(csv, "legacy,member,device,") {
		t.Fatalf("legacy record lost: %s %v", csv, err)
	}
	var version, arch sql.NullString
	if err := st.db.QueryRow("SELECT runtime_version, runtime_arch FROM sessions WHERE record_id='legacy'").Scan(&version, &arch); err != nil || version.Valid || arch.Valid {
		t.Fatalf("unknown legacy runtime fabricated: %v %v %v", version, arch, err)
	}
}

func TestIntrospectionRejectsServiceFailures(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, `{"error":"unauthorized"}`)
			}))
			defer upstream.Close()
			got := remoteIntrospect("agent-test", authOptions{URL: upstream.URL, ServiceToken: "service-test"})
			if !got.err {
				t.Fatalf("service HTTP %d must be retryable, got %+v", code, got)
			}
		})
	}
}

func TestIntrospectionCachesValidIdentity(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer service-test" {
			t.Error("missing service authentication")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["token"] != "agent-test" {
			t.Error("invalid introspection body")
		}
		fmt.Fprint(w, `{"active":true,"mid":"test-member","device_id":"test-device"}`)
	}))
	defer upstream.Close()
	auth := newIntrospector(authOptions{URL: upstream.URL, ServiceToken: "service-test"})
	for range 2 {
		if got := auth.introspect("agent-test"); !got.active || got.identity.MID != "test-member" {
			t.Fatalf("unexpected identity %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("expected one upstream call, got %d", calls)
	}
}

func TestIntrospectionDoesNotFollowRedirects(t *testing.T) {
	received := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		fmt.Fprint(w, `{"active":true,"mid":"test-member","device_id":"test-device"}`)
	}))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer upstream.Close()
	got := remoteIntrospect("agent-test", authOptions{URL: upstream.URL, ServiceToken: "service-test"})
	if received || !got.err {
		t.Fatalf("redirect must not forward Agent or service token: %+v", got)
	}
}

func TestUploadPartialValidationDedupAndIdentity(t *testing.T) {
	st := testStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"active":true,"mid":"real-member","device_id":"real-device"}`)
	}))
	defer upstream.Close()
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{URL: upstream.URL, ServiceToken: "service-test"})})
	body := `{"v":"0.1","batchId":"batch-test","records":[{"v":"0.1","type":"session","recordId":"record-test","mid":"spoof","deviceId":"spoof","receivedAt":"spoof","ip":"spoof","models":[{"cost":2,"currency":"CNY","turns":1},{"cost":3,"currency":"USD","turns":1}],"errors":[{"message":"test error","count":1}]},{"v":"99","type":"session","recordId":"bad-test"}]}`
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer agent-test")
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		if response.Code != 202 {
			t.Fatalf("upload failed: %d %s", response.Code, response.Body)
		}
		var result struct{ Accepted, Rejected int }
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Accepted != 1 || result.Rejected != 1 {
			t.Fatalf("bad counts: %s", response.Body)
		}
	}
	count, _ := st.countSessions()
	if count != 1 {
		t.Fatalf("duplicate sessions %d", count)
	}
	var payload string
	var cny, usd float64
	if err := st.db.QueryRow("SELECT payload,cost_cny,cost_usd FROM sessions").Scan(&payload, &cny, &usd); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	json.Unmarshal([]byte(payload), &stored)
	if stored["mid"] != "real-member" || stored["deviceId"] != "real-device" || stored["receivedAt"] == "spoof" || stored["ip"] == "spoof" {
		t.Fatalf("payload trusts client identity: %s", payload)
	}
	if cny != 2 || usd != 3 {
		t.Fatalf("currency mixed: %g %g", cny, usd)
	}
	var inserted int
	st.db.QueryRow("SELECT SUM(inserted) FROM uploads").Scan(&inserted)
	if inserted != 1 {
		t.Fatalf("insert logs wrong %d", inserted)
	}
	var n int
	st.db.QueryRow("SELECT COUNT(*) FROM devices").Scan(&n)
	if n != 1 {
		t.Fatalf("devices count %d", n)
	}
}

func TestUploadMissingTokenAndBodyLimit(t *testing.T) {
	st := testStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"active":true,"mid":"test-member","device_id":"test-device"}`)
	}))
	defer upstream.Close()
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{URL: upstream.URL, ServiceToken: "service-test"})})
	for _, tc := range []struct {
		body   string
		token  string
		status int
	}{{"{}", "", 401}, {"{", "test", 400}, {`{"records":[]}`, "test", 400}, {strings.Repeat("a", envelopeMaxBytes+1), "test", 413}} {
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(tc.body))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		res := httptest.NewRecorder()
		server.Handler.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("expected %d got %d", tc.status, res.Code)
		}
	}
}

func TestHealthAndMethodBoundaries(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/healthz", 200}, {"POST", "/healthz", 405}, {"GET", "/v1/sessions", 200}, {"DELETE", "/v1/sessions", 405}, {"GET", "/unknown", 404}} {
		res := httptest.NewRecorder()
		server.Handler.ServeHTTP(res, httptest.NewRequest(tc.method, tc.path, nil))
		if res.Code != tc.status {
			t.Fatalf("%s %s = %d", tc.method, tc.path, res.Code)
		}
	}
}

func TestRetentionRollupAndCSV(t *testing.T) {
	st := testStore(t)
	payload := map[string]any{"v": "0.1", "type": "session", "recordId": "old-test", "turns": float64(2), "tools": []any{map[string]any{"name": "sys", "calls": float64(3)}}, "errors": []any{map[string]any{"message": "private test error", "count": float64(2)}}}
	now := time.Now().UTC()
	_, err := st.insertBatch([]insertRow{{recordID: "old-test", mid: "test-member", deviceID: "test-device", receivedAt: utcNow(now.Add(-181 * 24 * time.Hour)), payload: payload}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.logUpload(uploadLog{receivedAt: utcNow(now.Add(-91 * 24 * time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if err := st.runRetention(); err != nil {
		t.Fatal(err)
	}
	var raw string
	st.db.QueryRow("SELECT payload FROM sessions").Scan(&raw)
	if strings.Contains(raw, "private test error") {
		t.Fatal("expired error retained")
	}
	if err := st.refreshRollup(); err != nil {
		t.Fatal(err)
	}
	var sessions, calls, turns int
	if err := st.db.QueryRow("SELECT sessions,tool_calls,turns FROM daily_rollup").Scan(&sessions, &calls, &turns); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || calls != 3 || turns != 2 {
		t.Fatalf("bad rollup %d %d %d", sessions, calls, turns)
	}
	var uploads int
	st.db.QueryRow("SELECT COUNT(*) FROM uploads").Scan(&uploads)
	if uploads != 0 {
		t.Fatal("expired uploads retained")
	}
	csv, err := st.exportSessionsCsv()
	if err != nil || !strings.Contains(csv, "old-test") {
		t.Fatalf("bad CSV %s %v", csv, err)
	}
	if bytes.Contains([]byte(csv), []byte("private test error")) {
		t.Fatal("CSV exposed errors")
	}
	removed, err := st.deleteByDevice("test-device")
	if err != nil || removed != 1 {
		t.Fatalf("delete %d %v", removed, err)
	}
}
