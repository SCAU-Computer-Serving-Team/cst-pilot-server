# CST Pilot Server

CST Pilot 的服务端仓库：接收队员工具包上报的使用情况遥测，存储在队伍服务器上。

当前只含一个服务：遥测接收端（`src/`）。数据契约以主仓库 cst-pilot 为准：`doc/contract.md` 与 `doc/telemetry/`，本仓库改动接收逻辑时同步主仓库文档。

## 收集什么

只采工具包自身的运行信息，不采机主数据与对话内容。一场会话产生一条记录：

| 类别 | 内容 |
|---|---|
| 会话 | 起止时间、时长、活跃时长、提问与轮次数、起止原因、通道（TUI / Web） |
| 模型 | 供应商、模型、思考档位、输入输出与缓存 token、估算费用与币种 |
| 工具 | 工具名与子功能、调用次数、失败与降级次数、耗时、结果体积与截断次数 |
| 上下文 | 压缩次数与规模、上下文峰值 |
| 失败 | 供应商状态码、网络错误、取消次数 |
| 环境 | 系统版本与架构、是否管理员、工具包版本 |
| 身份（服务端补充） | 队员编号、设备标识、接收时间、来源 IP 网段 |

## 不收集什么

- 对话正文、系统提示词、模型输出
- 工具参数值与输出正文：搜索词、URL、文件路径、命令正文
- 会话名、计算机名、用户名
- 模型凭据原文、硬件序列号

例外：turn 级的报错会完整收集。

## 数据去向与保留

- 同时落在 CSTOA 服务器与 Tim 的 `timserver_1`，不经过第三方数据接收服务。
- 会话记录长期保留；上传日志保留 90 天。

## 部署

| 接收端 | 地址 | 身份与运行 |
|---|---|---|
| CSTOA | `https://www.cstoa.top/api/telemetry` | systemd、nginx、真实 OA 内省 |
| Tim | `https://8.163.28.9:8445/api/telemetry` | 独立 Docker Compose、专用 CA、真实 OA 内省，见[部署说明](deploy/timserver_1/README.md) |

客户端同时向两端发送同一会话记录，两端分别存储、确认和去重。真实队员登录上传、异机备份及完整恢复演练待验收。
## 身份解析

OA 已提供 `POST /api/oauth/introspect`。生产配置内省地址与服务凭据，接收端据此校验真实 Agent 令牌并取得学号、设备标识和失效原因。CSTOA 使用本机地址，Tim 使用 OA 的公开 HTTPS 地址。

## 构建与运行

Go + 纯 Go SQLite 驱动（modernc.org/sqlite），单二进制 11MB，常驻内存约 12MB（目标机是 2G 内存的 Linux）。

```bash
# Windows 交叉编译 Linux 产物
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o telemetry-receiver ./src

# 常驻服务（生产须配置 OA 内省地址与服务凭据）
TELEMETRY_PORT=8787 TELEMETRY_DB=/var/lib/cst-telemetry/telemetry.db OA_INTROSPECT_URL=https://www.cstoa.top/api/oauth/introspect OA_SERVICE_TOKEN=<服务凭据> ./telemetry-receiver serve

# 其余子命令：export-csv [out] | rollup | delete --mid <id> | delete --device <id>
```

环境变量：`TELEMETRY_PORT`、`TELEMETRY_HOST`、`TELEMETRY_DB`、`OA_INTROSPECT_URL`（未配置时用桩）、`OA_SERVICE_TOKEN`。
