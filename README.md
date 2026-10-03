# CST Pilot Server

> CST Pilot 的服务端组件仓库，部署在队伍服务器上，不随工具包发行。

## 组成

| 服务 | 位置 | 说明 |
|---|---|---|
| 遥测接收端 | `src/telemetry/` | 收队员工具包上报的会话记录，SQLite 存储，规格见主仓库 `doc/telemetry/receiver/SPEC.md` |

## 运行

需要 Node.js 22.5+（`node:sqlite`）。零 npm 依赖，无需 install。

```bash
# 常驻服务（默认 127.0.0.1:8787，桩内省：令牌格式 stub-<mid>-<device>）
node --experimental-strip-types src/telemetry/main.ts serve

# 导出会话明细 CSV
node --experimental-strip-types src/telemetry/main.ts export-csv [out.csv]

# 手动重算 daily_rollup
node --experimental-strip-types src/telemetry/main.ts rollup

# 按队员或设备删除记录
node --experimental-strip-types src/telemetry/main.ts delete --mid M1024
node --experimental-strip-types src/telemetry/main.ts delete --device <id>
```

环境变量：

```
TELEMETRY_PORT=8080
TELEMETRY_HOST=127.0.0.1
TELEMETRY_DB=/var/lib/cst-telemetry/telemetry.db
OA_INTROSPECT_URL=https://cstoa.top/api/oauth/introspect   # 未配置时用桩
OA_SERVICE_TOKEN=<接收端服务凭据>
```

## 契约与文档

数据契约以主仓库（`cst-pilot`）为准：`doc/contract.md`、`doc/telemetry/`。本仓库改动接收逻辑时须同步主仓库文档，契约版本一起改。
