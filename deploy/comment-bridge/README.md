# 评论桥接 Docker 部署

此部署只把 `shadowbrain-comment-bridge` 二进制放进镜像。SQLite 数据库、飞书密钥、YouTube OAuth 文件和 B 站 Cookie 始终由目标主机外挂，绝不会进入构建上下文或镜像。

## 构建镜像

在仓库根目录执行：

```sh
docker buildx build \
  --platform linux/arm64 \
  --file deploy/comment-bridge/Dockerfile \
	--tag shadowbrain-comment-bridge:0.1.13 \
  --load .

docker image inspect shadowbrain-comment-bridge:0.1.13 \
  --format '{{.Os}}/{{.Architecture}}'
```

预期架构是 `linux/arm64`。`Dockerfile.dockerignore` 会在发送构建上下文前排除 dotenv、数据库、OAuth/Cookie、私钥和本地二进制。

## 外挂目录

部署目录必须由运行用户独占：

```text
comment-bridge/
├── compose.yaml
├── compose.env       # 镜像、UID/GID、卷名；0600
├── runtime.env       # 应用配置；0600
├── secrets/          # 0700
│   ├── feishu-app-secret
│   ├── youtube-client.json
│   ├── youtube-token.json
├── bilibili-auth/    # 0700；运行时可原子更新 B 站凭据
│   └── credential.json
├── staging/          # 切换时暂存一致性 SQLite 快照；0700
└── backups/          # 回滚快照；0700
```

三个静态 secret 文件都必须是当前用户拥有的普通 `0600` 文件。B 站凭据单独放在当前用户拥有的 `0700` `bilibili-auth/` 目录中，文件为普通 `0600`；Compose 将整个目录以可写 bind mount 挂入容器，供扫码成功后原子替换 `credential.json`。SQLite 使用 Docker named volume，避免在 macOS 文件共享层直接运行 WAL。其余 secret 仍以只读单文件 bind mount 挂入容器。

`runtime.env` 可以沿用现有 `.env.local`。Compose 会强制把五个文件路径覆盖为容器路径，因此无需把宿主绝对路径写进镜像。不要用 Compose `secrets:`：它通常会以 `0444`/root 呈现，不符合应用的严格文件校验。

运行时若 B 站接口返回未登录码 `-101` 或 CSRF 失效码 `-111`，服务会向飞书群发送一次凭据告警并可发送 180 秒有效的登录二维码；同一事故不会重复刷屏。扫码登录会强校验账号 ID，成功后原子更新 `bilibili-auth/credential.json`，随后恢复监控，无需手工复制 Cookie。二维码过期后，引用二维码消息并回复 `重新登录` 可申请新的二维码。二维码公开发送到目标群是明确选择；不把 Cookie、二维码原文或上游响应写入日志。应用需要“获取与上传图片或文件资源”权限 `im:resource`，新增权限后必须重新发布应用，否则二维码无法上传。`-101` 可由成功轮询重新布防，`-111` 必须由成功回复或点赞确认恢复。启动时文件缺失、权限错误或格式损坏仍会启动失败。风控、限流和普通网络错误不会触发该告警。

## 配置检查

```sh
cp compose.env.example compose.env
chmod 600 compose.env runtime.env
chmod 700 secrets bilibili-auth staging backups
chmod 600 secrets/*

# 首次迁移或新主机初始化：只从受控旧主机复制凭据，不要把内容放入镜像。
mkdir -p -m 700 bilibili-auth
cp /受控路径/credential.json bilibili-auth/credential.json
chmod 600 bilibili-auth/credential.json

docker volume create shadowbrain-comment-bridge-data-v1

docker compose --env-file compose.env -f compose.yaml config --quiet
```

数据卷声明为 `external`，因此 `docker compose down -v` 也不会删除它。镜像和 Compose 默认使用固定的非 root UID/GID `10001:10001`；首次挂载空卷时，Docker 会从镜像中复制 `/var/lib/shadowbrain` 的 `0700` 所有权元数据。上线前必须在目标 Docker Desktop 上实测该 UID 能写卷、能写入 `bilibili-auth/`，并能读取三个静态 `0600` bind mount 与 B 站 `0600` 凭据文件。

服务没有入站 HTTP 端口，不应配置 `ports`。健康检查只验证进程能以只读方式打开 SQLite 并读取 outbox 状态；飞书长连接和两个平台的真实可达性还需端到端验收。

## 数据导入与切换

同一数据库绝不能同时运行两个实例。最终切换顺序：

1. 在旧实例仍运行时预建目标目录、加载镜像并验证三个静态 secret 与 `bilibili-auth/credential.json` 的容器内视图均为普通 `0600` 文件；`bilibili-auth` 目录为 `0700` 且容器用户可写。
2. 停止旧实例并确认进程退出。
3. 用 SQLite `.backup` 生成最终一致性快照；校验 `PRAGMA integrity_check`、`foreign_key_check`、source/comment/outbox 聚合。
4. 用同一镜像、`network=none` 和非 root UID/GID，把快照作为 `/var/lib/shadowbrain/comment-bridge.db` 导入 named volume 并设为 `0600`。不要复制旧 `-wal`/`-shm`。同时把旧主机的 `bilibili-auth/` 目录（目录 `0700`，凭据 `0600`）复制到新主机；凭据目录必须独立备份，不能放入 SQLite 快照或镜像。
5. 启动容器，等待 `healthy`，确认版本、数据库聚合、飞书长连接及一轮平台轮询。
6. 完成真实的“新评论 → 飞书通知 → 引用回复 → 平台回复”测试，并分别验证 B 站 👍 与 YouTube 👍 手动提示。

`unknown` 动作永远不得自动重试。若切换后出现数据库差异、认证失败、意外 `pending/sending/unknown` 或容器无法健康，应先停止新容器，再用切换前 SQLite 快照**和对应的 `bilibili-auth/` 目录备份**恢复旧实例；切勿只恢复数据库而留下新 Cookie，也切勿让两个实例同时运行。回滚前后的备份均须同时覆盖 `backups/` 下的 SQLite 快照与 B 站凭据目录。

启动和状态检查：

```sh
docker compose --env-file compose.env -f compose.yaml up -d
docker compose --env-file compose.env -f compose.yaml ps --all
docker compose --env-file compose.env -f compose.yaml exec comment-bridge \
  /app/shadowbrain-comment-bridge status \
  --db /var/lib/shadowbrain/comment-bridge.db
```

停止但保留数据：

```sh
docker compose --env-file compose.env -f compose.yaml down
```

数据卷是 external volume，Compose 不会代为删除；仍不要手工执行 `docker volume rm`。
