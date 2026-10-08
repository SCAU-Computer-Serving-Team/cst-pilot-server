# timserver_1 遥测部署

接收端运行在 `timserver_1`（`8.163.28.9`），使用与 CSTOA 接收端相同的 Go 二进制。部署目录 `/srv/cst-pilot-server`，独立 Docker Compose 项目 `cst-pilot-telemetry`，不改动 EverOS 与 Qdrant。

## 入口与存储

| 项 | 值 |
|---|---|
| 上传 | `https://8.163.28.9:8445/api/telemetry` |
| 健康 | `https://8.163.28.9:8445/healthz` |
| Go 接收端 | Docker 网络内 `receiver:8787`，不映射宿主端口 |
| HTTPS 网关 | 独立 Caddy 容器，仅映射 TCP 8445 |
| 数据库 | `/srv/cst-pilot-server/data/telemetry.db` |
| 身份 | OA `https://www.cstoa.top/api/oauth/introspect`，服务凭据只放在服务器 |
| 安全组 | 仅为新服务增加 TCP 8445 入站，规则说明 `CST Pilot telemetry HTTPS 8445` |

## 部署

1. 将本目录中的部署文件和已验证的 Linux 二进制复制到 `/srv/cst-pilot-server`，二进制命名 `telemetry-receiver`。`.dockerignore` 只允许二进制与 Dockerfile 进入构建上下文。
2. 执行 `sh prepare.sh`。它创建数据目录、专用 CA 与服务证书，并安装备份和证书定时任务。
3. 配置 `telemetry.env`，参考示例文件。权限设为 `0600`，不要提交服务凭据。
4. 启动并核查：

```sh
cd /srv/cst-pilot-server
docker compose --env-file telemetry.env up -d --build --wait
docker compose --env-file telemetry.env ps
curl --cacert tls/ca/ca.crt https://8.163.28.9:8445/healthz
```

两个容器使用非 root 用户、只读根文件系统、删除全部 Linux capabilities，并设置失败重启。Go 进程与网关分别限制 64 MiB 内存。

## TLS

| 文件或任务 | 用途 |
|---|---|
| `tls/ca/ca.crt` | 专用公开 CA，复制到客户端扩展的 `timserver_1.crt` |
| `tls/ca/ca.key` | CA 私钥，仅服务器 root 可读，不进入客户端或镜像 |
| `tls/server/server.crt`、`server.key` | 服务证书与私钥，证书包含 IP `8.163.28.9` |
| `cst-telemetry-tls.timer` | 每日检查，剩余不足 30 天时续签 90 天证书并重启本项目网关 |

CA 有效期 10 年。客户端只对 Tim 上报端点使用此 CA，并继续校验证书有效期与 IP。更新 CA 需要同步发行客户端；普通服务证书续期不需要更新客户端。私钥只留在服务器，异机安全备份待安排。

## 备份与维护

`cst-telemetry-backup.timer` 每日 03:45 执行在线一致性备份，保留 30 天。备份在 `/srv/cst-pilot-server/backups`，并执行 SQLite `quick_check`。同机备份不覆盖整机或磁盘丢失，异机备份与完整恢复演练待完成。

```sh
systemctl start cst-telemetry-backup.service
systemctl list-timers --all | grep cst-telemetry
docker compose --env-file telemetry.env logs --tail 50
```

Go 服务自身在启动和每 24 小时执行一次保留期清理、每日汇总。

## 停止与回退

```sh
cd /srv/cst-pilot-server
docker compose --env-file telemetry.env down
systemctl disable --now cst-telemetry-backup.timer cst-telemetry-tls.timer
```

该操作保留数据库、证书和备份，不影响其他 Compose 项目。停止长期使用时删除 TCP 8445 的对应安全组规则，并从客户端配置移除 Tim 端点。
