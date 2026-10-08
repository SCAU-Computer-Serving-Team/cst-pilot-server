# CSTOA 接收端部署

Go 接收端由 `cst-telemetry.service` 托管，监听 `127.0.0.1:8787`。nginx 将 `https://www.cstoa.top/api/telemetry` 转发到 `/v1/sessions`，更新接收端不改动 OA 或 nginx。

| 项 | 路径或配置 |
|---|---|
| 程序与发布信息 | `/opt/cst-pilot-server/telemetry-receiver`、`BUILD-INFO.json` |
| 凭据 | `/opt/cst-pilot-server/telemetry.env`，更新时保持原文件 |
| 数据库 | `/var/lib/cst-telemetry/telemetry.db` |
| 内省 | `http://127.0.0.1:8080/api/oauth/introspect` |
| 每日备份 | `cst-telemetry-backup.timer`，04:15 执行，保留 30 天 |
| 备份目录 | `/var/lib/cst-telemetry/backups` |
| 回退程序与配置 | `/opt/cst-pilot-server/releases/<版本>-previous` |

## 更新

构建与发布步骤见[发布说明](../../doc/release.md)。在服务器执行候选包中的更新程序：

```sh
python3 /root/<候选目录>/deploy.py cstoa --candidate /root/<候选目录>
```

更新前执行在线一致性备份。程序以临时文件替换，再重启遥测服务；健康检查验证提交版本，检查已有会话和凭据保持不变。失败时恢复旧程序、服务配置并重启。

服务由 `csttele` 用户运行，只允许写入遥测数据目录。内省服务凭据不进入源码、构建包或日志。备份只保护本机数据，异机备份和完整恢复演练另行安排。
