/**
 * 遥测接收端 · 入口。
 *
 * 用法：
 *   node src/telemetry/main.ts serve              常驻服务（默认）
 *   node src/telemetry/main.ts export-csv [out]   导出会话明细 CSV
 *   node src/telemetry/main.ts delete --mid M1024 | --device <id>
 *   node src/telemetry/main.ts rollup             手动重算 daily_rollup
 *
 * 环境变量：
 *   TELEMETRY_PORT    监听端口（默认 8787，只绑 127.0.0.1 之外的地址需显式 TELEMETRY_HOST）
 *   TELEMETRY_HOST    监听地址（默认 127.0.0.1）
 *   TELEMETRY_DB      SQLite 文件路径（默认 ./telemetry.db）
 *   OA_INTROSPECT_URL OA 内省接口；未配置时用桩（令牌格式 stub-<mid>-<device>）
 *   OA_SERVICE_TOKEN  内省用的服务凭据
 */

import { writeFile } from "node:fs/promises";
import type { IntrospectOptions } from "./auth.ts";
import { createTelemetryServer } from "./server.ts";
import { TelemetryStore } from "./store.ts";

const DAY_MS = 86_400_000;

function envOptions(): { port: number; host: string; dbPath: string; auth: IntrospectOptions } {
	return {
		port: Number(process.env.TELEMETRY_PORT ?? 8787),
		host: process.env.TELEMETRY_HOST ?? "127.0.0.1",
		dbPath: process.env.TELEMETRY_DB ?? "telemetry.db",
		auth: {
			url: process.env.OA_INTROSPECT_URL,
			serviceToken: process.env.OA_SERVICE_TOKEN,
		},
	};
}

async function serve(): Promise<void> {
	const { port, host, dbPath, auth } = envOptions();
	const store = new TelemetryStore(dbPath);
	const server = createTelemetryServer({ store, auth });
	await new Promise<void>((resolve) => server.listen(port, host, resolve));
	console.log(`telemetry receiver listening on http://${host}:${port} (db: ${dbPath})`);
	if (!auth.url) console.log("identity: stub mode (token format stub-<mid>-<device>)");

	// 每日：保留期清理 + rollup 重算。启动时先跑一轮。
	const daily = () => {
		try {
			store.runRetention();
			store.refreshRollup();
		} catch (error) {
			console.error("daily maintenance failed:", error);
		}
	};
	daily();
	const timer = setInterval(daily, DAY_MS);
	timer.unref?.();

	const shutdown = () => {
		server.close(() => {
			store.close();
			process.exit(0);
		});
	};
	process.on("SIGINT", shutdown);
	process.on("SIGTERM", shutdown);
}

async function main(): Promise<void> {
	const [command, ...args] = process.argv.slice(2);
	if (command === "export-csv") {
		const store = new TelemetryStore(envOptions().dbPath);
		const csv = store.exportSessionsCsv();
		store.close();
		const out = args[0] ?? "telemetry-sessions.csv";
		await writeFile(out, csv, "utf8");
		console.log(`exported ${csv.split("\n").length - 1} rows to ${out}`);
		return;
	}
	if (command === "delete") {
		const mid = valueOf(args, "--mid");
		const device = valueOf(args, "--device");
		const store = new TelemetryStore(envOptions().dbPath);
		const removed = mid ? store.deleteByMid(mid) : device ? store.deleteByDevice(device) : 0;
		store.refreshRollup();
		store.close();
		console.log(`deleted ${removed} session rows`);
		return;
	}
	if (command === "rollup") {
		const store = new TelemetryStore(envOptions().dbPath);
		store.refreshRollup();
		store.close();
		console.log("rollup refreshed");
		return;
	}
	await serve();
}

function valueOf(args: string[], flag: string): string | undefined {
	const index = args.indexOf(flag);
	return index >= 0 ? args[index + 1] : undefined;
}

main().catch((error) => {
	console.error(error);
	process.exit(1);
});
