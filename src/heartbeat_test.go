package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestHeartbeatOrderingExpiryAndTerminalClose(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{}), now: func() time.Time { return now }})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	owner := "df79039e-3744-44c8-9754-7a9568ae5684"
	send := func(sequence int, state, expected string) {
		t.Helper()
		body := fmt.Sprintf(`{"v":"0.2","records":[],"heartbeat":{"runId":"ordered","sequence":%d,"sentAt":%q,"status":%q,"identity":{"installationId":%q,"environmentId":"env"},"lastActivityAt":null,"snapshots":[]}}`, sequence, utcNow(now), state, owner)
		response := signedUpload(t, server, key, owner, body)
		var result struct {
			Ack heartbeatAck `json:"heartbeatAck"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 202 || (result.Ack.Status != expected && result.Ack.Reason != expected) {
			t.Fatalf("heartbeat %d/%s = %d %s", sequence, state, response.Code, response.Body)
		}
	}
	send(2, "busy", "stored")
	first := utcNow(now)
	now = now.Add(60 * time.Second)
	send(2, "idle", "duplicate")
	send(1, "idle", "stale")
	run, err := st.readRun("ordered", now)
	if err != nil || run.State != "busy" || run.LastReceivedAt != first {
		t.Fatalf("duplicates refreshed state: %+v %v", run, err)
	}
	now = now.Add(31 * time.Second)
	run, err = st.readRun("ordered", now)
	if err != nil || run.State != "offline" {
		t.Fatalf("TTL read = %+v %v", run, err)
	}
	if err := st.sweepRuns(now); err != nil {
		t.Fatal(err)
	}
	send(3, "idle", "stored")
	send(4, "closed", "stored")
	send(5, "busy", "already-closed")
	run, err = st.readRun("ordered", now)
	if err != nil || run.State != "closed" || run.Sequence != 4 {
		t.Fatalf("terminal close = %+v %v", run, err)
	}
	// 接收端重启后仍保留关闭状态，另一个安装不能更新该运行 ID。
	server = newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{}), now: func() time.Time { return now }})
	owner = "b1f77e2a-4efb-41fc-9ff4-e4a116b68959"
	send(6, "idle", "owner-mismatch")
	if err := st.sweepRuns(now.Add(91 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.readRun("ordered", now); err == nil {
		t.Fatal("run state outlived retention")
	}
	if count, _ := st.countSessions(); count != 0 {
		t.Fatal("offline sweep manufactured usage")
	}
}

func TestExpiredHeartbeatDoesNotRejectDurableStats(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{}), now: func() time.Time { return now }})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	owner := "df79039e-3744-44c8-9754-7a9568ae5684"
	for i, age := range []time.Duration{91 * time.Second, -31 * time.Second} {
		body := fmt.Sprintf(`{"v":"0.2","records":[{"v":"0.2","type":"session","recordId":"backfill","revision":%d,"final":false,"endReason":"","identity":{"installationId":%q}}],"heartbeat":{"runId":"expired","sequence":%d,"sentAt":%q,"status":"idle","identity":{"installationId":%q},"lastActivityAt":null,"snapshots":[]}}`, i+1, owner, i+1, utcNow(now.Add(-age)), owner)
		response := signedUpload(t, server, key, owner, body)
		var result struct {
			Acks []snapshotAck `json:"acks"`
			Ack  heartbeatAck  `json:"heartbeatAck"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 202 || len(result.Acks) != 1 || result.Acks[0].Status != "stored" || result.Ack.Reason != "expired-heartbeat" {
			t.Fatalf("backfill = %d %s", response.Code, response.Body)
		}
	}
	if _, err := st.readRun("expired", now); err == nil {
		t.Fatal("expired heartbeat created online state")
	}
	if count, _ := st.countSessions(); count != 1 {
		t.Fatalf("backfill sessions = %d", count)
	}
}

func TestMalformedHeartbeatDoesNotRejectDurableStats(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	owner := "df79039e-3744-44c8-9754-7a9568ae5684"
	for i, heartbeat := range []string{`[]`, `"invalid"`, `42`, `null`, `{}`} {
		body := fmt.Sprintf(`{"v":"0.2","records":[{"v":"0.2","type":"session","recordId":"malformed-heartbeat","revision":%d,"final":false,"identity":{"installationId":%q}}],"heartbeat":%s}`, i+1, owner, heartbeat)
		response := signedUpload(t, server, key, owner, body)
		var result struct {
			Acks []snapshotAck `json:"acks"`
			Ack  heartbeatAck  `json:"heartbeatAck"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 202 || len(result.Acks) != 1 || result.Acks[0].Status != "stored" || result.Ack.Reason != "invalid-heartbeat" {
			t.Fatalf("malformed heartbeat %s rejected stats: %d %s", heartbeat, response.Code, response.Body)
		}
	}
	if count, _ := st.countSessions(); count != 1 {
		t.Fatalf("session count = %d", count)
	}
}

func TestPureHeartbeatDoesNotCreateSession(t *testing.T) {
	st := testStore(t)
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(authOptions{})})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	installation := "df79039e-3744-44c8-9754-7a9568ae5684"
	body := fmt.Sprintf(`{"v":"0.2","records":[],"heartbeat":{"runId":"run-a","sequence":1,"sentAt":%q,"status":"idle","identity":{"installationId":%q,"environmentId":"env-a"},"lastActivityAt":null,"snapshots":[]}}`, utcNow(time.Now()), installation)
	response := signedUpload(t, server, key, installation, body)
	var result struct {
		HeartbeatAck struct {
			RunID    string `json:"runId"`
			Sequence int    `json:"sequence"`
			Status   string `json:"status"`
		} `json:"heartbeatAck"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 202 || result.HeartbeatAck.RunID != "run-a" || result.HeartbeatAck.Sequence != 1 || result.HeartbeatAck.Status != "stored" {
		t.Fatalf("heartbeat = %d %s (%v)", response.Code, response.Body, err)
	}
	if count, _ := st.countSessions(); count != 0 {
		t.Fatalf("heartbeat created %d sessions", count)
	}
}
