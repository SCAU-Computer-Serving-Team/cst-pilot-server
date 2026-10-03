/**
 * 遥测接收端 · 身份解析。
 *
 * OA 内省接口未实现前用桩顶替：令牌格式 stub-<mid>-<device_id> 直接解析；
 * OA_INTROSPECT_URL 配置后切换为真内省。结果按令牌 SHA-256 缓存 60 秒，只存内存。
 */

import { createHash } from "node:crypto";

export interface Identity {
	mid: string;
	deviceId: string;
}

export type IntrospectResult =
	| { active: true; identity: Identity }
	| { active: false; reason: "expired" | "invalid" | "revoked" | "password-changed" }
	| { error: "unavailable" };

export interface IntrospectOptions {
	url?: string;
	serviceToken?: string;
}

const CACHE_TTL_MS = 60_000;
const cache = new Map<string, { at: number; result: IntrospectResult }>();

export async function introspect(token: string, options: IntrospectOptions): Promise<IntrospectResult> {
	const key = createHash("sha256").update(token).digest("hex");
	const hit = cache.get(key);
	if (hit && Date.now() - hit.at < CACHE_TTL_MS) return hit.result;
	const result = options.url ? await remoteIntrospect(token, options) : stubIntrospect(token);
	if (cache.size > 512) cache.clear();
	cache.set(key, { at: Date.now(), result });
	return result;
}

/** 桩：仅接受 stub-<mid>-<device> 形状，供联调与 e2e 使用。 */
function stubIntrospect(token: string): IntrospectResult {
	const match = /^stub-([A-Za-z0-9]+)-([A-Za-z0-9_-]+)$/.exec(token);
	if (!match) return { active: false, reason: "invalid" };
	return { active: true, identity: { mid: match[1], deviceId: match[2] } };
}

async function remoteIntrospect(token: string, options: IntrospectOptions): Promise<IntrospectResult> {
	try {
		const controller = new AbortController();
		const timer = setTimeout(() => controller.abort(), 5000);
		try {
			const response = await fetch(options.url!, {
				method: "POST",
				headers: {
					"Content-Type": "application/json",
					...(options.serviceToken ? { Authorization: `Bearer ${options.serviceToken}` } : {}),
				},
				body: JSON.stringify({ token }),
				signal: controller.signal,
			});
			if (response.status >= 500) return { error: "unavailable" };
			const payload = (await response.json()) as {
				active?: boolean;
				mid?: string;
				device_id?: string;
				reason?: string;
			};
			if (payload.active === true && payload.mid && payload.device_id) {
				return { active: true, identity: { mid: payload.mid, deviceId: payload.device_id } };
			}
			if (payload.reason === "revoked" || payload.reason === "password-changed") {
				return { active: false, reason: payload.reason };
			}
			return { active: false, reason: payload.reason === "expired" ? "expired" : "invalid" };
		} finally {
			clearTimeout(timer);
		}
	} catch {
		return { error: "unavailable" };
	}
}
