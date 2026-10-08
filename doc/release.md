# 发布接收端

发布以提交版本为单位，两台服务器使用同一个 Linux 二进制。上传协议与数据库结构保持兼容；更新不替换凭据、数据库或客户端信任的 CA。

## 本地验证

```powershell
go test ./... -count=1 -timeout 30s
go vet ./...
python -m unittest discover -s scripts -p 'test_*.py' -v
```

本地测试的 `TEMP`、`TMP` 与 `GOTMPDIR` 指向当天临时目录。CI 另执行 Go race 检查和 Linux 构建。

## 构建与更新

1. 提交改动，推送分支，创建 PR。CI 通过后合并到 `main`。
2. 在干净的已提交仓库构建。输出目录使用当天临时目录：

```powershell
./scripts/build-release.ps1 -OutputDirectory E:/tmp/<日期>/receiver-release
```

脚本生成 Linux 二进制、部署文件、备份脚本和 `BUILD-INFO.json`，输出 tar.gz 路径。程序内写入提交版本和 UTC 构建时间，元数据记录二进制 SHA-256。

3. 将同一个 tar.gz 上传到 `cstoa`、`timserver_1`，在各机独立候选目录解包。只复制构建包，不复制本机配置或数据库。
4. 执行候选目录中的更新脚本：

```sh
python3 /root/<候选目录>/deploy.py cstoa --candidate /root/<候选目录>
python3 /root/<候选目录>/deploy.py timserver_1 --candidate /root/<候选目录>
```

每条命令只在对应服务器执行。脚本要求接收端已有配置和数据库，执行在线一致性备份，保存旧程序及配置，再替换程序、重启遥测服务、验证版本与数据。失败时恢复旧接收端。回退不恢复数据库快照，不覆盖更新期间的有效上传。

CSTOA 的更新不重启 OA 或 nginx。Tim 的更新只操作 `cst-pilot-telemetry` Compose 项目，保留 TLS 私钥和公开 CA。

## 部署确认

| 检查 | 判据 |
|---|---|
| `/healthz` | `ok=true`，`version` 与提交一致，`builtAt` 与构建时间一致 |
| 二进制 | 两台机器 SHA-256 与构建元数据一致 |
| 数据 | 更新前的会话 `record_id` 全部仍存在 |
| 凭据与 CA | 文件哈希保持不变，不打印文件内容 |
| 上传鉴权 | 无效令牌拒绝；服务内省故障返回 503，客户端保留队列 |
| 备份 | 定时任务启用，备份可通过 SQLite `quick_check` |

同机备份不覆盖整机丢失。异机备份、完整恢复及真实队员双端上传仍须独立验收。
