# CST Pilot Server

CST Pilot 的遥测接收端。客户端同时向 CSTOA 与 Tim 的 `timserver_1` 上传会话统计，两端独立存储、确认和去重。程序使用 Go 与纯 Go SQLite 驱动，编译为单个可执行文件。

## 收集与保留

字段定义以主仓库 [信息收集契约](https://github.com/SCAU-Computer-Serving-Team/cst-pilot/blob/main/doc/contract.md) 和 [遥测规格](https://github.com/SCAU-Computer-Serving-Team/cst-pilot/tree/main/doc/telemetry) 为准。

| 类别 | 内容 |
|---|---|
| 会话 | 起止、时长、提问与模型往返、TUI/Web、结束原因 |
| 模型 | 供应商、模型、思考档位、token、估算费用与币种 |
| 工具 | 名称与子功能、调用、失败、降级、耗时和输出体积 |
| 上下文与环境 | 压缩、峰值、系统版本、架构、管理员权限 |
| 异常 | 网络错误、供应商状态、取消、模型调用报错原文 |
| 服务端身份 | OA 内省取得的学号、设备标识、接收时间和来源网段 |

正常字段不收对话、系统提示词、模型输出、工具参数与输出正文、路径、用户名或模型凭据。模型调用报错原文为例外，可能包含敏感片段。

会话统计长期保留，报错原文 180 天后移除，上传日志保留 90 天。服务启动时和每 24 小时执行清理与汇总。数据只落在上述两个接收端。

## 接口与身份

| 接口 | 用途 |
|---|---|
| `POST /v1/sessions` | OA Agent 令牌鉴权、批量校验、入库，按 `recordId` 去重 |
| `GET /healthz` | 健康、会话条数、提交版本与构建时间 |

生产须配置 `OA_INTROSPECT_URL` 与 `OA_SERVICE_TOKEN`。内省使用 HTTPS，或本机回环 HTTP；不跟随重定向。服务故障返回 503 且不缓存故障，令牌无效返回 401/403。身份字段在数据库列和原始 `payload` 中均来自验证结果。

本地测试身份须显式设置 `TELEMETRY_ALLOW_STUB=1`，生产不启用。

## 部署

| 接收端 | 上报入口 | 管理方式 |
|---|---|---|
| CSTOA | `https://www.cstoa.top/api/telemetry` | systemd、nginx，[部署说明](deploy/cstoa/README.md) |
| Tim | `https://8.163.28.9:8445/api/telemetry` | 独立 Docker Compose、专用 CA，[部署说明](deploy/timserver_1/README.md) |

两端均使用真实 OA 内省和每日在线 SQLite 备份。服务密钥与 TLS 私钥只在服务器保存，不进入仓库、镜像构建上下文或发行包。

发布、版本核对与回退见[发布说明](doc/release.md)。异机备份、完整恢复和真实队员双端成功入库须独立验收。

## 开发

```powershell
go test ./... -count=1 -timeout 30s
go vet ./...
python -m unittest discover -s scripts -p 'test_*.py' -v
```

| 目录 | 职责 |
|---|---|
| `src/` | 接收、身份验证、存储、汇总、保留期与 Go 测试 |
| `scripts/` | 构建发布包、更新已有部署、SQLite 备份及测试 |
| `deploy/` | 两个服务器的程序配置、定时任务和运维说明 |
| `doc/` | 发布与验证规则 |

命令：`serve`、`version`、`export-csv [out]`、`rollup`、`delete --mid <学号>`、`delete --device <设备>`。导出与删除需要显式设置 `TELEMETRY_DB`，删除只用于经过确认的运维操作。
