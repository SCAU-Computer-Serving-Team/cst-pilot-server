package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotOwnershipFinalityAndFirstReceipt(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	owners := []string{"df79039e-3744-44c8-9754-7a9568ae5684", "b1f77e2a-4efb-41fc-9ff4-e4a116b68959"}
	for _, step := range []struct {
		owner    int
		revision int
		final    bool
		status   string
	}{{0, 1, false, "stored"}, {1, 2, false, "owner-mismatch"}, {0, 2, true, "stored"}, {0, 3, false, "already-final"}} {
		reason := ""
		if step.final {
			reason = "quit"
		}
		body := fmt.Sprintf(`{"v":"0.2","records":[{"v":"0.2","type":"session","recordId":"owned","revision":%d,"final":%t,"endReason":%q,"identity":{"installationId":%q}}]}`, step.revision, step.final, reason, owners[step.owner])
		response := signedUpload(t, server, key, owners[step.owner], body)
		expected := `"status":"` + step.status + `"`
		if step.status == "owner-mismatch" || step.status == "already-final" {
			expected = `"reason":"` + step.status + `"`
		}
		if response.Code != 202 || !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("step %+v = %d %s", step, response.Code, response.Body)
		}
		if step.revision == 1 {
			// 固定首次接收日，后续版本不迁移汇总日期。
			var p map[string]any
			var raw string
			st.db.QueryRow("SELECT payload FROM sessions WHERE record_id='owned'").Scan(&raw)
			json.Unmarshal([]byte(raw), &p)
			p["receivedAt"] = "2026-10-01T00:00:00.000Z"
			data, _ := json.Marshal(p)
			if _, err := st.db.Exec("UPDATE sessions SET received_at=?, payload=? WHERE record_id='owned'", p["receivedAt"], string(data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var received string
	if err := st.db.QueryRow("SELECT received_at FROM sessions WHERE record_id='owned'").Scan(&received); err != nil || received != "2026-10-01T00:00:00.000Z" {
		t.Fatalf("first receipt = %s, %v", received, err)
	}
}

func TestUnknownSnapshotInstallationRemainsBoundToSigningOwner(t *testing.T) {
	for _, identity := range []string{`{"installationId":null}`, `{"installationId":""}`, `{}`} {
		t.Run(identity, func(t *testing.T) {
			st := testStore(t)
			server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			owner := "df79039e-3744-44c8-9754-7a9568ae5684"
			body := fmt.Sprintf(`{"v":"0.2","records":[{"v":"0.2","type":"session","recordId":"unknown-installation","revision":1,"final":false,"identity":%s}]}`, identity)
			response := signedUpload(t, server, key, owner, body)
			if response.Code != 202 || !strings.Contains(response.Body.String(), `"status":"stored"`) {
				t.Fatalf("unknown identity rejected: %d %s", response.Code, response.Body)
			}
			var storedOwner, raw string
			if err := st.db.QueryRow("SELECT owner_installation,payload FROM sessions WHERE record_id='unknown-installation'").Scan(&storedOwner, &raw); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(payload["identity"])
			if storedOwner != owner || string(encoded) != identity {
				t.Fatalf("owner/unknown identity changed: %s %s", storedOwner, encoded)
			}
			body = strings.Replace(body, `"revision":1`, `"revision":2`, 1)
			response = signedUpload(t, server, key, "b1f77e2a-4efb-41fc-9ff4-e4a116b68959", body)
			if response.Code != 202 || !strings.Contains(response.Body.String(), `"reason":"owner-mismatch"`) {
				t.Fatalf("signing ownership lost: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestSnapshotCapabilitiesAndInstallationProof(t *testing.T) {
	st := testStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"active":true,"mid":"test-member","device_id":"test-device"}`)
	}))
	defer upstream.Close()
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{URL: upstream.URL, ServiceToken: "service-test"})})
	res := httptest.NewRecorder()
	server.Handler.ServeHTTP(res, httptest.NewRequest("GET", "/v1/sessions", nil))
	if res.Code != 200 || !strings.Contains(res.Body.String(), "revision-acks") {
		t.Fatalf("capabilities = %d %s", res.Code, res.Body)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	installation := "df79039e-3744-44c8-9754-7a9568ae5684"
	body := `{"v":"0.2","records":[{"v":"0.2","type":"session","recordId":"proof","revision":1,"final":false,"endReason":"","identity":{"installationId":"` + installation + `"}}]}`
	response := signedUpload(t, server, key, installation, body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer test")
		r.Header.Set("X-Telemetry-Installation", installation)
	})
	if response.Code != 202 || !strings.Contains(response.Body.String(), `"status":"stored"`) {
		t.Fatalf("dual proof = %d %s", response.Code, response.Body)
	}
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(strings.ReplaceAll(body, "proof", "no-proof")))
	req.Header.Set("Authorization", "Bearer test")
	res = httptest.NewRecorder()
	server.Handler.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatalf("missing proof = %d %s", res.Code, res.Body)
	}
}

func TestCumulativeSnapshotsReplaceWithoutCountingAnotherSession(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	installation := "df79039e-3744-44c8-9754-7a9568ae5684"
	for _, step := range []struct {
		revision int
		turns    int
		status   string
	}{{1, 2, "stored"}, {2, 5, "stored"}, {1, 2, "stale"}, {2, 5, "duplicate"}} {
		body := fmt.Sprintf(`{"v":"0.2","records":[{"v":"0.2","type":"session","recordId":"cumulative","revision":%d,"final":false,"sessionId":"pi-session","startedAt":"2026-10-26T10:00:00+08:00","endReason":"","turns":%d,"identity":{"installationId":%q,"environmentId":"env-a"}}]}`, step.revision, step.turns, installation)
		response := signedUpload(t, server, key, installation, body)
		var result struct {
			V    string `json:"v"`
			Acks []struct {
				RecordID string `json:"recordId"`
				Revision int    `json:"revision"`
				Status   string `json:"status"`
			} `json:"acks"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 202 || result.V != "0.2" || len(result.Acks) != 1 {
			t.Fatalf("snapshot response = %d %s (%v)", response.Code, response.Body, err)
		}
		ack := result.Acks[0]
		if ack.RecordID != "cumulative" || ack.Revision != step.revision || ack.Status != step.status {
			t.Fatalf("ack = %+v, want %+v", ack, step)
		}
	}
	if count, _ := st.countSessions(); count != 1 {
		t.Fatalf("sessions = %d, want 1", count)
	}
	if err := st.refreshRollup(); err != nil {
		t.Fatal(err)
	}
	var sessions, turns int
	if err := st.db.QueryRow("SELECT sessions, turns FROM daily_rollup").Scan(&sessions, &turns); err != nil || sessions != 1 || turns != 5 {
		t.Fatalf("rollup = %d/%d, %v", sessions, turns, err)
	}
	csv, err := st.exportSessionsCsv()
	if err != nil || !strings.Contains(csv, "cumulative") {
		t.Fatalf("CSV = %s, %v", csv, err)
	}
}
