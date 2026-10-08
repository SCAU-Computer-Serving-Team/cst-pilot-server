package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	}{{"GET", "/healthz", 200}, {"POST", "/healthz", 405}, {"GET", "/v1/sessions", 405}, {"GET", "/unknown", 404}} {
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
