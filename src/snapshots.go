package main

import (
	"database/sql"
	"errors"
	"reflect"
)

type snapshotAck struct {
	RecordID string  `json:"recordId"`
	Revision float64 `json:"revision"`
	Status   string  `json:"status"`
	Reason   string  `json:"reason,omitempty"`
}

func (s *store) migrateSnapshots() error {
	for _, field := range []struct{ name, kind string }{
		{"snapshot_revision", "INTEGER NOT NULL DEFAULT 0"},
		{"snapshot_final", "INTEGER NOT NULL DEFAULT 0"},
		{"owner_installation", "TEXT NOT NULL DEFAULT ''"},
		{"updated_at", "TEXT NOT NULL DEFAULT ''"},
	} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name=?", field.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := s.db.Exec("ALTER TABLE sessions ADD COLUMN " + field.name + " " + field.kind); err != nil {
				return err
			}
		}
	}
	return nil
}

// 写入与版本检查在同一事务内完成；更新保留首次归属和接收日。
func (s *store) applySnapshots(rows []insertRow, installation string) ([]snapshotAck, int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	acks := make([]snapshotAck, 0, len(rows))
	inserted := 0
	for _, row := range rows {
		ack, fresh, err := s.applySnapshot(tx, row, installation)
		if err != nil {
			return nil, 0, err
		}
		acks = append(acks, ack)
		if fresh {
			inserted++
		}
	}
	return acks, inserted, tx.Commit()
}

func (s *store) applySnapshot(tx *sql.Tx, row insertRow, installation string) (snapshotAck, bool, error) {
	p := row.payload
	updatedAt := row.receivedAt
	ack := snapshotAck{RecordID: row.recordID, Revision: num(p["revision"]), Status: "stored"}
	var owner, oldPayload string
	var revision float64
	var final bool
	err := tx.QueryRow("SELECT owner_installation, snapshot_revision, snapshot_final, payload FROM sessions WHERE record_id=?", row.recordID).Scan(&owner, &revision, &final, &oldPayload)
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return ack, false, err
	}
	if !fresh {
		var old map[string]any
		if err := jsonUnmarshal([]byte(oldPayload), &old); err != nil {
			return ack, false, err
		}
		reject := func(reason string) (snapshotAck, bool, error) {
			ack.Status, ack.Reason = "rejected", reason
			return ack, false, nil
		}
		if owner == "" || str(old["v"]) != "0.2" {
			return reject("immutable-legacy-record")
		}
		if owner != installation {
			return reject("owner-mismatch")
		}
		for _, field := range []string{"identity", "os", "runtime", "admin", "sessionId", "startedAt", "channel", "reason", "kitVersion"} {
			if !reflect.DeepEqual(old[field], p[field]) {
				return reject("instance-mismatch")
			}
		}
		if ack.Revision <= revision {
			ack.Status = "stale"
			if ack.Revision == revision {
				ack.Status = "duplicate"
			}
			return ack, false, nil
		}
		if final {
			return reject("already-final")
		}
		row.mid, row.deviceID = str(old["mid"]), str(old["deviceId"])
		row.receivedAt, row.ipNet = str(old["receivedAt"]), str(old["ip"])
		// 同一事务中替换完整派生行，读取方只见到更新前或更新后的完整版本。
		if _, err := tx.Exec("DELETE FROM sessions WHERE record_id=?", row.recordID); err != nil {
			return ack, false, err
		}
	}
	if _, err := s.insertOne(tx, row); err != nil {
		return ack, false, err
	}
	_, err = tx.Exec("UPDATE sessions SET owner_installation=?, snapshot_revision=?, snapshot_final=?, updated_at=? WHERE record_id=?", installation, ack.Revision, p["final"], updatedAt, row.recordID)
	return ack, fresh, err
}
