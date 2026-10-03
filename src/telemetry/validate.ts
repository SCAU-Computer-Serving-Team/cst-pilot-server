/**
 * 遥测接收端 · 校验。
 *
 * 整批拒绝与逐条拒收的规则见 doc/telemetry/receiver/SPEC.md「校验」。
 * 拒一条就是永久丢一条，条件取最保守的一组；其余一律宽容。
 */

export const ACCEPTED_VERSIONS = ["0.1"];
export const ENVELOPE_MAX_RECORDS = 50;
export const ENVELOPE_MAX_BYTES = 1024 * 1024;
export const MAX_TOOLS = 512;
export const MAX_MODELS = 32;
export const MAX_ERRORS = 10;

export interface EnvelopeProblem {
	status: number;
	code: string;
}

/** 整批级校验；通过后返回 records 数组（元素类型交由逐条校验放宽处理）。 */
export function validateEnvelope(body: unknown): { records: unknown[] } | EnvelopeProblem {
	if (body === null || typeof body !== "object" || Array.isArray((body as { records?: unknown }).records) === false) {
		return { status: 400, code: "invalid_body" };
	}
	const records = (body as { records: unknown[] }).records;
	if (records.length === 0) return { status: 400, code: "empty_batch" };
	if (records.length > ENVELOPE_MAX_RECORDS) return { status: 400, code: "too_many_records" };
	return { records };
}

/** 逐条校验；返回拒绝 reason，null 表示收下。 */
export function validateRecord(record: unknown): string | null {
	if (record === null || typeof record !== "object") return "unknown-type";
	const r = record as Record<string, unknown>;
	if (typeof r.v !== "string" || !ACCEPTED_VERSIONS.includes(r.v)) return "unknown-version";
	if (r.type !== "session") return "unknown-type";
	if (typeof r.recordId !== "string" || r.recordId === "" || r.recordId.length > 64) return "bad-record-id";
	if (Array.isArray(r.tools) && r.tools.length > MAX_TOOLS) return "too-many-elements";
	if (Array.isArray(r.models) && r.models.length > MAX_MODELS) return "too-many-elements";
	if (Array.isArray(r.errors) && r.errors.length > MAX_ERRORS) return "too-many-elements";
	return null;
}
