package main

import (
	"database/sql"
	"math"
	"time"
)

const onlineTTL = 90 * time.Second

type heartbeatAck struct {
	RunID    string  `json:"runId"`
	Sequence float64 `json:"sequence"`
	Status   string  `json:"status"`
	Reason   string  `json:"reason,omitempty"`
}

type processRun struct {
	RunID, InstallationID, EnvironmentID    string
	Sequence                                float64
	State, LastReceivedAt, LastTransitionAt string
}

func (s *store) migratePresence() error {
	return s.exec(`CREATE TABLE IF NOT EXISTS process_runs (
		run_id TEXT PRIMARY KEY, installation_id TEXT NOT NULL, environment_id TEXT NOT NULL,
		sequence INTEGER NOT NULL, state TEXT NOT NULL, last_received_at TEXT NOT NULL,
		last_transition_at TEXT NOT NULL, last_activity_at TEXT, snapshots TEXT NOT NULL
	); CREATE INDEX IF NOT EXISTS process_runs_received ON process_runs(last_received_at)`)
}

func safePositive(value any) bool {
	n, ok := value.(float64)
	return ok && n >= 1 && n <= 9007199254740991 && math.Trunc(n) == n
}

// 心跳仅更新运行状态；统计快照和上传记录均不参与在线期限计算。
func (s *store) applyHeartbeat(raw any, owner string, now time.Time) (heartbeatAck, error) {
	h, _ := raw.(map[string]any)
	ack := heartbeatAck{RunID: str(h["runId"]), Sequence: num(h["sequence"]), Status: "rejected"}
	identity, _ := h["identity"].(map[string]any)
	state := str(h["status"])
	snapshots, ok := h["snapshots"].([]any)
	if ack.RunID == "" || len(ack.RunID) > 64 || !safePositive(h["sequence"]) || !ok || len(snapshots) > 512 || (state != "idle" && state != "busy" && state != "closed") {
		ack.Reason = "invalid-heartbeat"
		return ack, nil
	}
	if str(identity["installationId"]) != owner || owner == "" {
		ack.Reason = "installation-mismatch"
		return ack, nil
	}
	stamp, err := time.Parse(time.RFC3339Nano, str(h["sentAt"]))
	if err != nil || now.Sub(stamp) > onlineTTL || stamp.Sub(now) > 30*time.Second {
		ack.Reason = "expired-heartbeat"
		return ack, nil
	}
	if activity := h["lastActivityAt"]; activity != nil {
		if _, err := time.Parse(time.RFC3339Nano, str(activity)); err != nil {
			ack.Reason = "invalid-heartbeat"
			return ack, nil
		}
	}
	for _, raw := range snapshots {
		snapshot, _ := raw.(map[string]any)
		if str(snapshot["recordId"]) == "" || len(str(snapshot["recordId"])) > 64 || !safePositive(snapshot["revision"]) {
			ack.Reason = "invalid-heartbeat"
			return ack, nil
		}
	}
	data, err := jsonMarshal(snapshots)
	if err != nil {
		return ack, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ack, err
	}
	defer tx.Rollback()
	var old processRun
	err = tx.QueryRow(`SELECT installation_id, environment_id, sequence, state, last_received_at, last_transition_at FROM process_runs WHERE run_id=?`, ack.RunID).Scan(&old.InstallationID, &old.EnvironmentID, &old.Sequence, &old.State, &old.LastReceivedAt, &old.LastTransitionAt)
	if err != nil && err != sql.ErrNoRows {
		return ack, err
	}
	transition := utcNow(now)
	if err == nil {
		switch {
		case old.InstallationID != owner:
			ack.Reason = "owner-mismatch"
		case old.EnvironmentID != str(identity["environmentId"]):
			ack.Reason = "environment-mismatch"
		case ack.Sequence == old.Sequence:
			ack.Status = "duplicate"
		case ack.Sequence < old.Sequence:
			ack.Status = "stale"
		case old.State == "closed":
			ack.Reason = "already-closed"
		}
		if ack.Status != "rejected" || ack.Reason != "" {
			return ack, nil
		}
		if effectiveRun(old, now).State == state {
			transition = old.LastTransitionAt
		}
	}
	_, err = tx.Exec(`INSERT INTO process_runs (run_id, installation_id, environment_id, sequence, state, last_received_at, last_transition_at, last_activity_at, snapshots) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET sequence=excluded.sequence, state=excluded.state, last_received_at=excluded.last_received_at, last_transition_at=excluded.last_transition_at, last_activity_at=excluded.last_activity_at, snapshots=excluded.snapshots`, ack.RunID, owner, str(identity["environmentId"]), ack.Sequence, state, utcNow(now), transition, nullableString(h["lastActivityAt"]), string(data))
	if err != nil {
		return ack, err
	}
	ack.Status = "stored"
	return ack, tx.Commit()
}

func effectiveRun(run processRun, now time.Time) processRun {
	received, err := time.Parse(time.RFC3339Nano, run.LastReceivedAt)
	if err == nil && (run.State == "idle" || run.State == "busy") && !now.Before(received.Add(onlineTTL)) {
		run.State, run.LastTransitionAt = "offline", utcNow(received.Add(onlineTTL))
	}
	return run
}

// 本地读取也计算超时，不依赖维护定时器是否已执行。
func (s *store) readRun(id string, now time.Time) (processRun, error) {
	run := processRun{RunID: id}
	err := s.db.QueryRow(`SELECT installation_id, environment_id, sequence, state, last_received_at, last_transition_at FROM process_runs WHERE run_id=?`, id).Scan(&run.InstallationID, &run.EnvironmentID, &run.Sequence, &run.State, &run.LastReceivedAt, &run.LastTransitionAt)
	return effectiveRun(run, now), err
}

func (s *store) sweepRuns(now time.Time) error {
	// SQLite strftime 与统一 UTC 毫秒格式保持一致。
	_, err := s.db.Exec(`UPDATE process_runs SET state='offline', last_transition_at=strftime('%Y-%m-%dT%H:%M:%fZ', last_received_at, '+90 seconds') WHERE state IN ('idle','busy') AND last_received_at <= ?`, utcNow(now.Add(-onlineTTL)))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM process_runs WHERE last_received_at < ?`, utcNow(now.Add(-90*24*time.Hour)))
	return err
}
