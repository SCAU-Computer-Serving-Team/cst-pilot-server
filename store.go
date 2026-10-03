// 遥测接收端 · 存储（modernc.org/sqlite，纯 Go 驱动）。
//
// 列是派生的，payload 是权威的：先写 payload，再从它抽列，同一事务完成。
// 表结构与保留期见主仓库 doc/telemetry/receiver/SPEC.md「存储」「保留期」。
// received_at 统一存 UTC ISO 字符串（同格式字典序即时间序，便于范围比较）。
package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type insertRow struct {
	recordID   string
	mid        string
	deviceID   string
	receivedAt string
	ipNet      string
	payload    map[string]any
}

type uploadLog struct {
	batchID        string
	receivedAt     string
	mid            string
	deviceID       string
	recordCount    int
	accepted       int
	rejected       int
	inserted       int
	rejectedDetail any
	statusCode     int
	errCode        string
	bodyBytes      int
	durationMs     int64
	ipNet          string
}

type store struct {
	db *sql.DB
}

// openStore 打开库并施加内存相关 PRAGMA：
// 页缓存上限 2MB、WAL、不占 mmap。单连接串行写，SQLite 场景下省一个锁层。
func openStore(path string) (*store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)&_pragma=mmap_size(0)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &store{db: db}
	if err := s.exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	record_id TEXT PRIMARY KEY,
	mid TEXT NOT NULL,
	device_id TEXT NOT NULL,
	received_at TEXT NOT NULL,
	ip_net TEXT NOT NULL DEFAULT '',
	v TEXT NOT NULL DEFAULT '',
	kit_version TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	end_reason TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL DEFAULT '',
	ended_at TEXT NOT NULL DEFAULT '',
	duration_ms REAL NOT NULL DEFAULT 0,
	active_ms REAL NOT NULL DEFAULT 0,
	prompts REAL NOT NULL DEFAULT 0,
	turns REAL NOT NULL DEFAULT 0,
	context_entries REAL,
	compactions REAL NOT NULL DEFAULT 0,
	compaction_tokens REAL NOT NULL DEFAULT 0,
	compaction_overflows REAL NOT NULL DEFAULT 0,
	compaction_failures REAL NOT NULL DEFAULT 0,
	context_peak REAL NOT NULL DEFAULT 0,
	context_window REAL NOT NULL DEFAULT 0,
	network_errors REAL NOT NULL DEFAULT 0,
	aborted REAL NOT NULL DEFAULT 0,
	tool_failures REAL NOT NULL DEFAULT 0,
	os_version TEXT NOT NULL DEFAULT '',
	os_arch TEXT NOT NULL DEFAULT '',
	admin INTEGER,
	cost_cny REAL NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	unpriced_turns REAL NOT NULL DEFAULT 0,
	payload TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_received_mid ON sessions(received_at, mid);
CREATE INDEX IF NOT EXISTS idx_sessions_device ON sessions(device_id, received_at);
CREATE INDEX IF NOT EXISTS idx_sessions_session ON sessions(session_id);
CREATE INDEX IF NOT EXISTS idx_sessions_kit ON sessions(kit_version);

CREATE TABLE IF NOT EXISTS devices (
	device_id TEXT PRIMARY KEY,
	first_seen_at TEXT NOT NULL,
	last_seen_at TEXT NOT NULL,
	os_version TEXT NOT NULL DEFAULT '',
	os_arch TEXT NOT NULL DEFAULT '',
	admin INTEGER,
	kit_version TEXT NOT NULL DEFAULT '',
	last_ip_net TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS members (
	mid TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	team TEXT NOT NULL DEFAULT '',
	active INTEGER NOT NULL DEFAULT 1,
	synced_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS uploads (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	batch_id TEXT NOT NULL DEFAULT '',
	received_at TEXT NOT NULL,
	mid TEXT NOT NULL DEFAULT '',
	device_id TEXT NOT NULL DEFAULT '',
	record_count INTEGER NOT NULL DEFAULT 0,
	accepted INTEGER NOT NULL DEFAULT 0,
	rejected INTEGER NOT NULL DEFAULT 0,
	inserted INTEGER NOT NULL DEFAULT 0,
	rejected_detail TEXT NOT NULL DEFAULT '[]',
	status_code INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	body_bytes INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	ip_net TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS daily_rollup (
	date TEXT NOT NULL,
	mid TEXT NOT NULL,
	sessions INTEGER NOT NULL DEFAULT 0,
	duration_ms REAL NOT NULL DEFAULT 0,
	active_ms REAL NOT NULL DEFAULT 0,
	prompts REAL NOT NULL DEFAULT 0,
	turns REAL NOT NULL DEFAULT 0,
	context_entries REAL NOT NULL DEFAULT 0,
	compactions REAL NOT NULL DEFAULT 0,
	compaction_overflows REAL NOT NULL DEFAULT 0,
	compaction_failures REAL NOT NULL DEFAULT 0,
	context_peak REAL NOT NULL DEFAULT 0,
	context_window REAL NOT NULL DEFAULT 0,
	tool_calls REAL NOT NULL DEFAULT 0,
	tool_failures REAL NOT NULL DEFAULT 0,
	degraded REAL NOT NULL DEFAULT 0,
	input_tokens REAL NOT NULL DEFAULT 0,
	output_tokens REAL NOT NULL DEFAULT 0,
	cache_read_tokens REAL NOT NULL DEFAULT 0,
	cache_write_tokens REAL NOT NULL DEFAULT 0,
	total_tokens REAL NOT NULL DEFAULT 0,
	cost_cny REAL NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	unpriced_turns REAL NOT NULL DEFAULT 0,
	aborted REAL NOT NULL DEFAULT 0,
	network_errors REAL NOT NULL DEFAULT 0,
	error_groups REAL NOT NULL DEFAULT 0,
	error_turns REAL NOT NULL DEFAULT 0,
	models TEXT NOT NULL DEFAULT '[]',
	tools TEXT NOT NULL DEFAULT '[]',
	PRIMARY KEY (date, mid)
);
`

func (s *store) exec(query string) error {
	_, err := s.db.Exec(query)
	return err
}

func num(v any) float64 {
	if f, ok := v.(float64); ok && !isNaNOrInf(f) {
		return f
	}
	return 0
}

func isNaNOrInf(f float64) bool {
	return f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308
}

func str(v any) string {
	if t, ok := v.(string); ok {
		return t
	}
	return ""
}

func recordArray(payload map[string]any, key string) []map[string]any {
	list, ok := payload[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// insertBatch 逐条入库；返回真正新增的行数（小于 accepted 即重发）。payload 是权威，列从它抽取。
func (s *store) insertBatch(rows []insertRow) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, row := range rows {
		ok, err := s.insertOne(tx, row)
		if err != nil {
			tx.Rollback()
			return 0, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

const insertSessionSQL = `INSERT OR IGNORE INTO sessions (
	record_id, mid, device_id, received_at, ip_net,
	v, kit_version, session_id, channel, reason, end_reason,
	started_at, ended_at, duration_ms, active_ms, prompts, turns, context_entries,
	compactions, compaction_tokens, compaction_overflows, compaction_failures,
	context_peak, context_window, network_errors, aborted, tool_failures,
	os_version, os_arch, admin, cost_cny, cost_usd, unpriced_turns, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func (s *store) insertOne(tx *sql.Tx, row insertRow) (bool, error) {
	p := row.payload
	var costCny, costUsd, unpricedTurns float64
	for _, model := range recordArray(p, "models") {
		cost := num(model["cost"])
		turns := num(model["turns"])
		switch model["currency"] {
		case "CNY":
			costCny += cost
		case "USD":
			costUsd += cost
		default:
			unpricedTurns += turns
		}
	}
	osInfo, _ := p["os"].(map[string]any)
	if osInfo == nil {
		osInfo = map[string]any{}
	}
	admin := any(nil)
	if b, ok := p["admin"].(bool); ok {
		if b {
			admin = 1
		} else {
			admin = 0
		}
	}
	var contextEntries any
	if v, ok := p["contextEntries"]; ok && v != nil {
		contextEntries = num(v)
	}
	payloadJSON, err := jsonMarshal(p)
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(insertSessionSQL,
		row.recordID, row.mid, row.deviceID, row.receivedAt, row.ipNet,
		str(p["v"]), str(p["kitVersion"]), str(p["sessionId"]), str(p["channel"]), str(p["reason"]), str(p["endReason"]),
		str(p["startedAt"]), str(p["endedAt"]), num(p["durationMs"]), num(p["activeMs"]), num(p["prompts"]), num(p["turns"]), contextEntries,
		num(p["compactions"]), num(p["compactionTokens"]), num(p["compactionOverflows"]), num(p["compactionFailures"]),
		num(p["contextPeak"]), num(p["contextWindow"]), num(p["networkErrors"]), num(p["aborted"]), num(p["toolFailures"]),
		str(osInfo["version"]), str(osInfo["arch"]), admin, costCny, costUsd, unpricedTurns, string(payloadJSON),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	return true, s.upsertDevice(tx, row, p, osInfo)
}

func (s *store) upsertDevice(tx *sql.Tx, row insertRow, p map[string]any, osInfo map[string]any) error {
	admin := any(nil)
	if b, ok := p["admin"].(bool); ok {
		if b {
			admin = 1
		} else {
			admin = 0
		}
	}
	_, err := tx.Exec(`INSERT INTO devices (device_id, first_seen_at, last_seen_at, os_version, os_arch, admin, kit_version, last_ip_net)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			os_version = excluded.os_version,
			os_arch = excluded.os_arch,
			admin = excluded.admin,
			kit_version = excluded.kit_version,
			last_ip_net = excluded.last_ip_net`,
		row.deviceID, row.receivedAt, row.receivedAt, str(osInfo["version"]), str(osInfo["arch"]), admin, str(p["kitVersion"]), row.ipNet)
	return err
}

func (s *store) logUpload(entry uploadLog) error {
	detail, err := jsonMarshal(entry.rejectedDetail)
	if err != nil {
		detail = []byte("[]")
	}
	_, err = s.db.Exec(`INSERT INTO uploads (batch_id, received_at, mid, device_id, record_count, accepted, rejected, inserted, rejected_detail, status_code, error, body_bytes, duration_ms, ip_net)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.batchID, entry.receivedAt, entry.mid, entry.deviceID, entry.recordCount, entry.accepted, entry.rejected,
		entry.inserted, string(detail), entry.statusCode, entry.errCode, entry.bodyBytes, entry.durationMs, entry.ipNet)
	return err
}

type rollupBucket struct {
	date                string
	mid                 string
	sessions            int64
	durationMs          float64
	activeMs            float64
	prompts             float64
	turns               float64
	contextEntries      float64
	compactions         float64
	compactionOverflows float64
	compactionFailures  float64
	peak                float64
	window              float64
	peakRatio           float64
	toolCalls           float64
	toolFailures        float64
	degraded            float64
	input               float64
	output              float64
	cacheRead           float64
	cacheWrite          float64
	totalTokens         float64
	costCny             float64
	costUsd             float64
	unpricedTurns       float64
	aborted             float64
	networkErrors       float64
	errorGroups         float64
	errorTurns          float64
	models              map[string]float64
	tools               map[string]float64
}

func newBucket(date, mid string) *rollupBucket {
	return &rollupBucket{date: date, mid: mid, peakRatio: -1, models: map[string]float64{}, tools: map[string]float64{}}
}

// refreshRollup 每日重算 rollup：按 +08:00 归日，全量重建（数据量小，字段完整优先）。
func (s *store) refreshRollup() error {
	rows, err := s.db.Query("SELECT received_at, mid, payload FROM sessions")
	if err != nil {
		return err
	}
	defer rows.Close()
	buckets := map[string]*rollupBucket{}
	for rows.Next() {
		var receivedAt, mid, payloadText string
		if err := rows.Scan(&receivedAt, &mid, &payloadText); err != nil {
			return err
		}
		t, err := time.Parse(time.RFC3339Nano, receivedAt)
		if err != nil {
			continue
		}
		day := t.Add(8 * time.Hour).UTC().Format("2006-01-02")
		var payload map[string]any
		if err := jsonUnmarshal([]byte(payloadText), &payload); err != nil {
			continue
		}
		key := day + "\x00" + mid
		bucket := buckets[key]
		if bucket == nil {
			bucket = newBucket(day, mid)
			buckets[key] = bucket
		}

		bucket.sessions++
		bucket.durationMs += num(payload["durationMs"])
		bucket.activeMs += num(payload["activeMs"])
		bucket.prompts += num(payload["prompts"])
		bucket.turns += num(payload["turns"])
		bucket.contextEntries += num(payload["contextEntries"])
		bucket.compactions += num(payload["compactions"])
		bucket.compactionOverflows += num(payload["compactionOverflows"])
		bucket.compactionFailures += num(payload["compactionFailures"])
		peak := num(payload["contextPeak"])
		windowSize := num(payload["contextWindow"])
		ratio := 0.0
		if windowSize > 0 {
			ratio = peak / windowSize
		}
		if ratio > bucket.peakRatio {
			bucket.peakRatio = ratio
			bucket.peak = peak
			bucket.window = windowSize
		}
		bucket.aborted += num(payload["aborted"])
		bucket.networkErrors += num(payload["networkErrors"])

		for _, tool := range recordArray(payload, "tools") {
			bucket.toolCalls += num(tool["calls"])
			bucket.toolFailures += num(tool["failures"])
			bucket.degraded += num(tool["degraded"])
			name := str(tool["name"])
			if scope, ok := tool["scope"]; ok && scope != nil {
				name += "(" + fmt.Sprintf("%v", scope) + ")"
			}
			bucket.tools[name] += num(tool["calls"])
		}
		for _, model := range recordArray(payload, "models") {
			bucket.input += num(model["input"])
			bucket.output += num(model["output"])
			bucket.cacheRead += num(model["cacheRead"])
			bucket.cacheWrite += num(model["cacheWrite"])
			bucket.totalTokens += num(model["totalTokens"])
			cost := num(model["cost"])
			switch model["currency"] {
			case "CNY":
				bucket.costCny += cost
			case "USD":
				bucket.costUsd += cost
			default:
				bucket.unpricedTurns += num(model["turns"])
			}
			key := str(model["provider"]) + "/" + str(model["model"]) + "/" + str(model["thinkingLevel"])
			bucket.models[key] += num(model["turns"])
		}
		errors := recordArray(payload, "errors")
		bucket.errorGroups += float64(len(errors))
		for _, e := range errors {
			bucket.errorTurns += num(e["count"])
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM daily_rollup"); err != nil {
		tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO daily_rollup (date, mid, sessions, duration_ms, active_ms, prompts, turns, context_entries,
		compactions, compaction_overflows, compaction_failures, context_peak, context_window,
		tool_calls, tool_failures, degraded, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens,
		cost_cny, cost_usd, unpriced_turns, aborted, network_errors, error_groups, error_turns, models, tools)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, bucket := range buckets {
		if _, err := stmt.Exec(bucket.date, bucket.mid, bucket.sessions, bucket.durationMs, bucket.activeMs, bucket.prompts,
			bucket.turns, bucket.contextEntries, bucket.compactions, bucket.compactionOverflows, bucket.compactionFailures,
			bucket.peak, bucket.window, bucket.toolCalls, bucket.toolFailures, bucket.degraded, bucket.input, bucket.output,
			bucket.cacheRead, bucket.cacheWrite, bucket.totalTokens, bucket.costCny, bucket.costUsd, bucket.unpricedTurns,
			bucket.aborted, bucket.networkErrors, bucket.errorGroups, bucket.errorTurns,
			distJSON(bucket.models, "turns"), distJSON(bucket.tools, "calls")); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

type nameCount struct {
	Name  string  `json:"name"`
	Turns float64 `json:"turns"`
	Calls float64 `json:"calls"`
}

// distJSON 把分布 map 序列化为 [{"name":..,"turns":..}] / [{"name":..,"calls":..}]，键排序保证输出稳定。
func distJSON(m map[string]float64, countKey string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]nameCount, 0, len(keys))
	for _, k := range keys {
		nc := nameCount{Name: k}
		if countKey == "turns" {
			nc.Turns = m[k]
		} else {
			nc.Calls = m[k]
		}
		out = append(out, nc)
	}
	b, err := jsonMarshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// runRetention 保留期：清 180 天前 payload 里的 errors；删 90 天前 uploads。同格式 ISO 字符串比较。
func (s *store) runRetention() error {
	errorsCutoff := time.Now().Add(-180 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	uploadsCutoff := time.Now().Add(-90 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	rows, err := s.db.Query("SELECT record_id, payload FROM sessions WHERE received_at < ?", errorsCutoff)
	if err != nil {
		return err
	}
	type stale struct {
		id      string
		payload string
	}
	var stales []stale
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			rows.Close()
			return err
		}
		stales = append(stales, stale{id: id, payload: payload})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range stales {
		var payload map[string]any
		if err := jsonUnmarshal([]byte(item.payload), &payload); err != nil {
			continue // 损坏行不动。
		}
		if _, ok := payload["errors"]; !ok {
			continue
		}
		delete(payload, "errors")
		updated, err := jsonMarshal(payload)
		if err != nil {
			continue
		}
		if _, err := s.db.Exec("UPDATE sessions SET payload = ? WHERE record_id = ?", string(updated), item.id); err != nil {
			return err
		}
	}
	_, err = s.db.Exec("DELETE FROM uploads WHERE received_at < ?", uploadsCutoff)
	return err
}

func (s *store) deleteByMid(mid string) (int64, error) {
	return s.deleteWhere("mid = ?", mid)
}

func (s *store) deleteByDevice(deviceID string) (int64, error) {
	return s.deleteWhere("device_id = ?", deviceID)
}

func (s *store) deleteWhere(where string, arg string) (int64, error) {
	result, err := s.db.Exec("DELETE FROM sessions WHERE "+where, arg)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

var csvHeader = []string{
	"record_id", "mid", "device_id", "received_at", "ip_net", "kit_version", "channel", "reason", "end_reason",
	"started_at", "ended_at", "duration_ms", "active_ms", "prompts", "turns", "context_entries",
	"compactions", "context_peak", "context_window", "network_errors", "aborted", "tool_failures",
	"os_version", "os_arch", "admin", "cost_cny", "cost_usd", "unpriced_turns",
}

// exportSessionsCsv 报表导出：会话明细 CSV（聚合看板后置，先给原始明细）。
func (s *store) exportSessionsCsv() (string, error) {
	columns := strings.Join(csvHeader, ", ")
	rows, err := s.db.Query("SELECT " + columns + " FROM sessions ORDER BY received_at")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	sb.WriteString(strings.Join(csvHeader, ","))
	sb.WriteByte('\n')
	raws := make([]any, len(csvHeader))
	scans := make([]any, len(csvHeader))
	for i := range raws {
		scans[i] = &raws[i]
	}
	for rows.Next() {
		for i := range raws {
			raws[i] = nil
		}
		if err := rows.Scan(scans...); err != nil {
			return "", err
		}
		for i, raw := range raws {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(csvField(raw))
		}
		sb.WriteByte('\n')
	}
	return sb.String(), rows.Err()
}

func csvField(raw any) string {
	var text string
	switch v := raw.(type) {
	case nil:
		text = ""
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		text = fmt.Sprintf("%v", v)
	}
	if strings.ContainsAny(text, ",\"\n") {
		return "\"" + strings.ReplaceAll(text, "\"", "\"\"") + "\""
	}
	return text
}

func (s *store) countSessions() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n)
	return n, err
}

func (s *store) close() error { return s.db.Close() }
