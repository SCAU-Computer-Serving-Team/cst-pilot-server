/**
 * 遥测接收端 · HTTP 服务。
 *
 * 一个写端点 POST /v1/sessions，一个 GET /healthz；不开对外读接口。
 * 单批上限：50 条或 1 MB，超出整批拒绝不截断（截断会让发送端把没送到的记录也删掉）。
 */

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { introspect, type IntrospectOptions } from "./auth.ts";
import { TelemetryStore } from "./store.ts";
import { ENVELOPE_MAX_BYTES, validateEnvelope, validateRecord } from "./validate.ts";

export interface ServerOptions {
	store: TelemetryStore;
	auth: IntrospectOptions;
}

function ipNetOf(ip: string): string {
	if (ip.includes(".") && !ip.includes(":")) {
		const parts = ip.split(".");
		if (parts.length === 4) return `${parts[0]}.${parts[1]}.${parts[2]}.0/24`;
	}
	return ip;
}

function sendJson(response: ServerResponse, status: number, body: unknown): void {
	const payload = JSON.stringify(body);
	response.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) });
	response.end(payload);
}

async function readBody(request: IncomingMessage): Promise<{ ok: true; body: string } | { ok: false; tooLarge: boolean }> {
	const declared = Number(request.headers["content-length"] ?? 0);
	if (declared > ENVELOPE_MAX_BYTES) return { ok: false, tooLarge: true };
	const chunks: Buffer[] = [];
	let total = 0;
	for await (const chunk of request) {
		total += (chunk as Buffer).length;
		if (total > ENVELOPE_MAX_BYTES) return { ok: false, tooLarge: true };
		chunks.push(chunk as Buffer);
	}
	return { ok: true, body: Buffer.concat(chunks).toString("utf8") };
}

export function createTelemetryServer(options: ServerOptions) {
	const { store } = options;
	const server = createServer((request, response) => {
		void handle(request, response).catch((error) => {
			sendJson(response, 500, { error: { code: "server_error", message: String(error) } });
		});
	});

	async function handle(request: IncomingMessage, response: ServerResponse): Promise<void> {
		if (request.method === "GET" && request.url === "/healthz") {
			sendJson(response, 200, { ok: true, sessions: store.countSessions() });
			return;
		}
		if (request.method === "POST" && request.url === "/v1/sessions") {
			await handleUpload(request, response);
			return;
		}
		sendJson(response, 404, { error: { code: "not_found", message: "no such endpoint" } });
	}

	async function handleUpload(request: IncomingMessage, response: ServerResponse): Promise<void> {
		const startedAt = Date.now();
		const ip = request.socket.remoteAddress ?? "";
		const ipNet = ipNetOf(ip.replace(/^::ffff:/, ""));
		const token = (request.headers.authorization ?? "").replace(/^Bearer\s+/i, "");

		const fail = (status: number, code: string, message: string) => {
			store.logUpload({
				batchId: "",
				receivedAt: new Date().toISOString(),
				mid: "",
				deviceId: "",
				recordCount: 0,
				accepted: 0,
				rejected: 0,
				inserted: 0,
				rejectedDetail: [],
				statusCode: status,
				error: code,
				bodyBytes: 0,
				durationMs: Date.now() - startedAt,
				ipNet,
			});
			sendJson(response, status, { error: { code, message } });
		};

		if (!token) return fail(401, "unauthorized", "missing bearer token");

		const identity = await introspect(token, options.auth);
		if (identity.error === "unavailable") return fail(503, "introspection_unavailable", "identity service down");
		if (!identity.active) {
			const code = identity.reason === "revoked" || identity.reason === "password-changed" ? "forbidden" : "unauthorized";
			return fail(code === "forbidden" ? 403 : 401, code, identity.reason);
		}

		const body = await readBody(request);
		if (!body.ok) return fail(body.tooLarge ? 413 : 400, "payload_too_large", "batch exceeds 1 MB");

		let parsed: unknown;
		try {
			parsed = JSON.parse(body.body);
		} catch {
			return fail(400, "invalid_json", "body is not valid JSON");
		}
		const envelope = validateEnvelope(parsed);
		if ("status" in envelope) return fail(envelope.status, envelope.code, "envelope rejected");

		const accepted: { recordId: string; payload: Record<string, unknown> }[] = [];
		const rejectedDetail: { index: number; recordId: string; reason: string }[] = [];
		const reasons: Record<string, number> = {};
		envelope.records.forEach((record, index) => {
			const reason = validateRecord(record);
			if (reason === null) {
				accepted.push({ recordId: String((record as Record<string, unknown>).recordId), payload: record as Record<string, unknown> });
			} else {
				const recordId =
					typeof (record as Record<string, unknown>)?.recordId === "string"
						? String((record as Record<string, unknown>).recordId)
						: "";
				rejectedDetail.push({ index, recordId, reason });
				reasons[reason] = (reasons[reason] ?? 0) + 1;
			}
		});

		const receivedAt = new Date().toISOString();
		let inserted = 0;
		if (accepted.length > 0) {
			inserted = store.insertBatch(
				accepted.map((entry) => ({
					recordId: entry.recordId,
					mid: identity.identity.mid,
					deviceId: identity.identity.deviceId,
					receivedAt,
					ipNet,
					payload: entry.payload,
				})),
			);
		}

		const batchId =
			typeof (parsed as Record<string, unknown>).batchId === "string"
				? String((parsed as Record<string, unknown>).batchId)
				: "";
		store.logUpload({
			batchId,
			receivedAt,
			mid: identity.identity.mid,
			deviceId: identity.identity.deviceId,
			recordCount: envelope.records.length,
			accepted: accepted.length,
			rejected: rejectedDetail.length,
			inserted,
			rejectedDetail,
			statusCode: 202,
			error: "",
			bodyBytes: Buffer.byteLength(body.body, "utf8"),
			durationMs: Date.now() - startedAt,
			ipNet,
		});

		sendJson(response, 202, { accepted: accepted.length, rejected: rejectedDetail.length, reasons });
	}

	return server;
}
