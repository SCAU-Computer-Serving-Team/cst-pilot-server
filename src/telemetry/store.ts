/**
 * 遥测接收端 · 存储（node:sqlite，同步 API）。
 *
 * 列是派生的，payload 是权威的：先写 payload，再从它抽列，同一事务完成。
 * 表结构与保留期见 doc/telemetry/receiver/SPEC.md「存储」「保留期」。
 * received_at 统一存 UTC ISO 字符串（同格式字典序即时间序，便于范围比较）。
 */

import { DatabaseSync } from "node:sqlite";

export interface InsertRow {
	recordId: string;
	mid: string;
	deviceId: string;
	receivedAt: string;
	ipNet: string;
	payload: Record<string, unknown>;
}

const SCHEMA = `
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
`;

interface RollupBucket {
	date: string;
	mid: string;
	sessions: number;
	durationMs: number;
	activeMs: number;
	prompts: number;
	turns: number;
	contextEntries: number;
	compactions: number;
	compactionOverflows: number;
	compactionFailures: number;
	peak: number;
	window: number;
	peakRatio: number;
	toolCalls: number;
	toolFailures: number;
	degraded: number;
	input: number;
	output: number;
	cacheRead: number;
	cacheWrite: number;
	totalTokens: number;
	costCny: number;
	costUsd: number;
	unpricedTurns: number;
	aborted: number;
	networkErrors: number;
	errorGroups: number;
	errorTurns: number;
	models: Map<string, number>;
	tools: Map<string, number>;
}

function emptyBucket(date: string, mid: string): RollupBucket {
	return {
		date,
		mid,
		sessions: 0,
		durationMs: 0,
		activeMs: 0,
		prompts: 0,
		turns: 0,
		contextEntries: 0,
		compactions: 0,
		compactionOverflows: 0,
		compactionFailures: 0,
		peak: 0,
		window: 0,
		peakRatio: -1,
		toolCalls: 0,
		toolFailures: 0,
		degraded: 0,
		input: 0,
		output: 0,
		cacheRead: 0,
		cacheWrite: 0,
		totalTokens: 0,
		costCny: 0,
		costUsd: 0,
		unpricedTurns: 0,
		aborted: 0,
		networkErrors: 0,
		errorGroups: 0,
		errorTurns: 0,
		models: new Map(),
		tools: new Map(),
	};
}

function num(value: unknown): number {
	return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function recordArray(payload: Record<string, unknown>, key: string): Record<string, unknown>[] {
	return Array.isArray(payload[key]) ? (payload[key] as Record<string, unknown>[]) : [];
}

export class TelemetryStore {
	private readonly db: DatabaseSync;

	constructor(dbPath: string) {
		this.db = new DatabaseSync(dbPath);
		this.db.exec("PRAGMA journal_mode = WAL");
		this.db.exec(SCHEMA);
	}

	/** 逐条入库；返回真正新增的行数（小于 accepted 即重发）。payload 是权威，列从它抽取。 */
	insertBatch(rows: InsertRow[]): number {
		let inserted = 0;
		this.db.exec("BEGIN");
		try {
			for (const row of rows) {
				if (this.insertOne(row)) inserted++;
			}
			this.db.exec("COMMIT");
		} catch (error) {
			this.db.exec("ROLLBACK");
			throw error;
		}
		return inserted;
	}

	private insertOne(row: InsertRow): boolean {
		const p = row.payload;
		let costCny = 0;
		let costUsd = 0;
		let unpricedTurns = 0;
		for (const model of recordArray(p, "models")) {
			const cost = num(model.cost);
			const turns = num(model.turns);
			if (model.currency === "CNY") costCny += cost;
			else if (model.currency === "USD") costUsd += cost;
			else unpricedTurns += turns;
		}
		const os = (p.os as { version?: unknown; arch?: unknown } | undefined) ?? {};
		const info = this.db
			.prepare(
				`INSERT OR IGNORE INTO sessions (
					record_id, mid, device_id, received_at, ip_net,
					v, kit_version, session_id, channel, reason, end_reason,
					started_at, ended_at, duration_ms, active_ms, prompts, turns, context_entries,
					compactions, compaction_tokens, compaction_overflows, compaction_failures,
					context_peak, context_window, network_errors, aborted, tool_failures,
					os_version, os_arch, admin, cost_cny, cost_usd, unpriced_turns, payload
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			)
			.run(
				row.recordId,
				row.mid,
				row.deviceId,
				row.receivedAt,
				row.ipNet,
				String(p.v ?? ""),
				String(p.kitVersion ?? ""),
				String(p.sessionId ?? ""),
				String(p.channel ?? ""),
				String(p.reason ?? ""),
				String(p.endReason ?? ""),
				String(p.startedAt ?? ""),
				String(p.endedAt ?? ""),
				num(p.durationMs),
				num(p.activeMs),
				num(p.prompts),
				num(p.turns),
				p.contextEntries === undefined ? null : num(p.contextEntries),
				num(p.compactions),
				num(p.compactionTokens),
				num(p.compactionOverflows),
				num(p.compactionFailures),
				num(p.contextPeak),
				num(p.contextWindow),
				num(p.networkErrors),
				num(p.aborted),
				num(p.toolFailures),
				String(os.version ?? ""),
				String(os.arch ?? ""),
				typeof p.admin === "boolean" ? (p.admin ? 1 : 0) : null,
				costCny,
				costUsd,
				unpricedTurns,
				JSON.stringify(p),
			);
		if (info.changes === 0) return false;
		this.upsertDevice(row, p, os);
		return true;
	}

	private upsertDevice(row: InsertRow, p: Record<string, unknown>, os: { version?: unknown; arch?: unknown }): void {
		this.db
			.prepare(
				`INSERT INTO devices (device_id, first_seen_at, last_seen_at, os_version, os_arch, admin, kit_version, last_ip_net)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(device_id) DO UPDATE SET
					last_seen_at = excluded.last_seen_at,
					os_version = excluded.os_version,
					os_arch = excluded.os_arch,
					admin = excluded.admin,
					kit_version = excluded.kit_version,
					last_ip_net = excluded.last_ip_net`,
			)
			.run(
				row.deviceId,
				row.receivedAt,
				row.receivedAt,
				String(os.version ?? ""),
				String(os.arch ?? ""),
				typeof p.admin === "boolean" ? (p.admin ? 1 : 0) : null,
				String(p.kitVersion ?? ""),
				row.ipNet,
			);
	}

	logUpload(entry: {
		batchId: string;
		receivedAt: string;
		mid: string;
		deviceId: string;
		recordCount: number;
		accepted: number;
		rejected: number;
		inserted: number;
		rejectedDetail: unknown;
		statusCode: number;
		error: string;
		bodyBytes: number;
		durationMs: number;
		ipNet: string;
	}): void {
		this.db
			.prepare(
				`INSERT INTO uploads (batch_id, received_at, mid, device_id, record_count, accepted, rejected, inserted, rejected_detail, status_code, error, body_bytes, duration_ms, ip_net)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			)
			.run(
				entry.batchId,
				entry.receivedAt,
				entry.mid,
				entry.deviceId,
				entry.recordCount,
				entry.accepted,
				entry.rejected,
				entry.inserted,
				JSON.stringify(entry.rejectedDetail ?? []),
				entry.statusCode,
				entry.error,
				entry.bodyBytes,
				entry.durationMs,
				entry.ipNet,
			);
	}

	/** 每日重算 rollup：按 +08:00 归日，JS 全量重建（数据量小，字段完整优先）。 */
	refreshRollup(): void {
		const rows = this.db.prepare("SELECT received_at, mid, payload FROM sessions").all() as {
			received_at: string;
			mid: string;
			payload: string;
		}[];
		const buckets = new Map<string, RollupBucket>();
		for (const row of rows) {
			const day = new Date(new Date(row.received_at).getTime() + 8 * 3600_000).toISOString().slice(0, 10);
			let payload: Record<string, unknown>;
			try {
				payload = JSON.parse(row.payload) as Record<string, unknown>;
			} catch {
				continue;
			}
			const key = `${day}\u0000${row.mid}`;
			const bucket = buckets.get(key) ?? emptyBucket(day, row.mid);
			buckets.set(key, bucket);

			bucket.sessions++;
			bucket.durationMs += num(payload.durationMs);
			bucket.activeMs += num(payload.activeMs);
			bucket.prompts += num(payload.prompts);
			bucket.turns += num(payload.turns);
			bucket.contextEntries += num(payload.contextEntries);
			bucket.compactions += num(payload.compactions);
			bucket.compactionOverflows += num(payload.compactionOverflows);
			bucket.compactionFailures += num(payload.compactionFailures);
			const peak = num(payload.contextPeak);
			const windowSize = num(payload.contextWindow);
			const ratio = windowSize > 0 ? peak / windowSize : 0;
			if (ratio > bucket.peakRatio) {
				bucket.peakRatio = ratio;
				bucket.peak = peak;
				bucket.window = windowSize;
			}
			bucket.aborted += num(payload.aborted);
			bucket.networkErrors += num(payload.networkErrors);

			for (const tool of recordArray(payload, "tools")) {
				bucket.toolCalls += num(tool.calls);
				bucket.toolFailures += num(tool.failures);
				bucket.degraded += num(tool.degraded);
				const name = `${String(tool.name ?? "")}${tool.scope === undefined ? "" : `(${String(tool.scope)})`}`;
				bucket.tools.set(name, (bucket.tools.get(name) ?? 0) + num(tool.calls));
			}
			for (const model of recordArray(payload, "models")) {
				bucket.input += num(model.input);
				bucket.output += num(model.output);
				bucket.cacheRead += num(model.cacheRead);
				bucket.cacheWrite += num(model.cacheWrite);
				bucket.totalTokens += num(model.totalTokens);
				const cost = num(model.cost);
				if (model.currency === "CNY") bucket.costCny += cost;
				else if (model.currency === "USD") bucket.costUsd += cost;
				else bucket.unpricedTurns += num(model.turns);
				const modelKey = `${String(model.provider ?? "")}/${String(model.model ?? "")}/${String(model.thinkingLevel ?? "")}`;
				bucket.models.set(modelKey, (bucket.models.get(modelKey) ?? 0) + num(model.turns));
			}
			const errors = recordArray(payload, "errors");
			bucket.errorGroups += errors.length;
			for (const error of errors) bucket.errorTurns += num(error.count);
		}

		this.db.exec("BEGIN");
		try {
			this.db.exec("DELETE FROM daily_rollup");
			const statement = this.db.prepare(
				`INSERT INTO daily_rollup (date, mid, sessions, duration_ms, active_ms, prompts, turns, context_entries,
					compactions, compaction_overflows, compaction_failures, context_peak, context_window,
					tool_calls, tool_failures, degraded, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens,
					cost_cny, cost_usd, unpriced_turns, aborted, network_errors, error_groups, error_turns, models, tools)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			);
			for (const bucket of buckets.values()) {
				statement.run(
					bucket.date,
					bucket.mid,
					bucket.sessions,
					bucket.durationMs,
					bucket.activeMs,
					bucket.prompts,
					bucket.turns,
					bucket.contextEntries,
					bucket.compactions,
					bucket.compactionOverflows,
					bucket.compactionFailures,
					bucket.peak,
					bucket.window,
					bucket.toolCalls,
					bucket.toolFailures,
					bucket.degraded,
					bucket.input,
					bucket.output,
					bucket.cacheRead,
					bucket.cacheWrite,
					bucket.totalTokens,
					bucket.costCny,
					bucket.costUsd,
					bucket.unpricedTurns,
					bucket.aborted,
					bucket.networkErrors,
					bucket.errorGroups,
					bucket.errorTurns,
					JSON.stringify([...bucket.models.entries()].map(([name, turns]) => ({ name, turns }))),
					JSON.stringify([...bucket.tools.entries()].map(([name, calls]) => ({ name, calls }))),
				);
			}
			this.db.exec("COMMIT");
		} catch (error) {
			this.db.exec("ROLLBACK");
			throw error;
		}
	}

	/** 保留期：清 180 天前 payload 里的 errors；删 90 天前 uploads。同格式 ISO 字符串比较。 */
	runRetention(): void {
		const errorsCutoff = new Date(Date.now() - 180 * 86_400_000).toISOString();
		const uploadsCutoff = new Date(Date.now() - 90 * 86_400_000).toISOString();
		const stale = this.db
			.prepare("SELECT record_id, payload FROM sessions WHERE received_at < ?")
			.all(errorsCutoff) as { record_id: string; payload: string }[];
		const update = this.db.prepare("UPDATE sessions SET payload = ? WHERE record_id = ?");
		for (const row of stale) {
			try {
				const payload = JSON.parse(row.payload) as Record<string, unknown>;
				if (payload.errors !== undefined) {
					delete payload.errors;
					update.run(JSON.stringify(payload), row.record_id);
				}
			} catch {
				// 损坏行不动。
			}
		}
		this.db.prepare("DELETE FROM uploads WHERE received_at < ?").run(uploadsCutoff);
	}

	deleteByMid(mid: string): number {
		const info = this.db.prepare("DELETE FROM sessions WHERE mid = ?").run(mid);
		return Number(info.changes);
	}

	deleteByDevice(deviceId: string): number {
		const info = this.db.prepare("DELETE FROM sessions WHERE device_id = ?").run(deviceId);
		return Number(info.changes);
	}

	/** 报表导出：会话明细 CSV（聚合看板后置，先给原始明细）。 */
	exportSessionsCsv(): string {
		const rows = this.db
			.prepare(
				`SELECT record_id, mid, device_id, received_at, ip_net, kit_version, channel, reason, end_reason,
					started_at, ended_at, duration_ms, active_ms, prompts, turns, context_entries,
					compactions, context_peak, context_window, network_errors, aborted, tool_failures,
					os_version, os_arch, admin, cost_cny, cost_usd, unpriced_turns
				 FROM sessions ORDER BY received_at`,
			)
			.all() as Record<string, unknown>[];
		const header = [
			"record_id",
			"mid",
			"device_id",
			"received_at",
			"ip_net",
			"kit_version",
			"channel",
			"reason",
			"end_reason",
			"started_at",
			"ended_at",
			"duration_ms",
			"active_ms",
			"prompts",
			"turns",
			"context_entries",
			"compactions",
			"context_peak",
			"context_window",
			"network_errors",
			"aborted",
			"tool_failures",
			"os_version",
			"os_arch",
			"admin",
			"cost_cny",
			"cost_usd",
			"unpriced_turns",
		];
		const escape = (value: unknown) => {
			const text = value === null || value === undefined ? "" : String(value);
			return /[",\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
		};
		return [header.join(","), ...rows.map((row) => header.map((key) => escape(row[key])).join(","))].join("\n");
	}

	countSessions(): number {
		return Number((this.db.prepare("SELECT COUNT(*) AS n FROM sessions").get() as { n: number }).n);
	}

	close(): void {
		this.db.close();
	}
}
