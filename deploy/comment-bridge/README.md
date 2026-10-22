# 评论桥接 Docker 部署

此部署只把 `shadowbrain-comment-bridge` 二进制放进镜像。SQLite 数据库、飞书密钥、YouTube OAuth 文件和 B 站 Cookie 始终由目标主机外挂，绝不会进入构建上下文或镜像。

## 构建镜像

在仓库根目录执行：

```sh
docker buildx build \
  --platform linux/arm64 \
  --file deploy/comment-bridge/Dockerfile \
	--tag shadowbrain-comment-bridge:0.1.19 \
  --load .

docker image inspect shadowbrain-comment-bridge:0.1.19 \
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

## YouTube 独立回复群

0.1.18 起，在 `runtime.env` 设置 `SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID` 可使用同一应用机器人向另一个群发送 YouTube 通知并接收引用回复。机器人必须已加入该群；保留原 `SHADOWBRAIN_FEISHU_CHAT_ID`，B站和告警仍走原群。使用此模式时清空 `SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE`，Compose 参数不再包含 `compose.youtube-webhook.yaml`。不迁移或重置数据库，旧 Webhook 消息不补发。回滚到 0.1.17 时恢复原 Webhook 配置及 Compose 参数，保留当前数据库和 B站风控状态。

0.1.19 首次启动会在事务中移除 `feishu_messages` 的评论级唯一约束，保留消息 ID 唯一性、全部旧行及外键关系，用于人工授权重发后的多消息映射。上线前备份并在副本验证迁移；正常通知去重仍由 `notification_jobs` 保证。0.1.18 可读取迁移后的库，回滚保留当前库及新消息映射，不恢复旧快照丢弃已发送动作。

## YouTube 独立 Webhook

将完整 URL 保存在 `secrets/youtube-feishu-webhook`，由部署用户拥有且权限为 `0600`。该文件只通过可选 Compose 覆盖文件只读挂载，不进入镜像。未启用分流的部署不需要这个文件。

启用分流时，后续所有配置检查、启动和升级命令都同时传入两个 Compose 文件，例如：

```sh
docker compose --env-file compose.env -f compose.yaml -f compose.youtube-webhook.yaml config --quiet
docker compose --env-file compose.env -f compose.yaml -f compose.youtube-webhook.yaml up -d
```

该覆盖文件只增加 YouTube Webhook 的文件路径与挂载。切换前将它和 Webhook 文件纳入配置/凭据备份，回滚时恢复对应配置。无需迁移数据库；已送达的 Webhook 通知没有双向消息映射，不能通过引用回复或表情触发平台动作。

## 弹幕分析工作器

弹幕抓取仍在唯一的评论桥接容器里执行，与评论共用 B 站请求门禁；宿主机只分析本地队列，不另行请求 B 站。此选项需要可信、已登录 Codex 的宿主机，不为用户共享或公网服务提供 AI 认证。

1. 构建与宿主 OS/架构匹配的同版本二进制，放到部署目录的 `bin/`，保留旧版本供回滚。无需将 Codex 或其登录文件打进镜像。
2. 创建 `danmaku-queue/`（`0700`），验证宿主用户及现有容器 UID 均可读写其中的 `0600` 文件。macOS Docker 文件共享的权限映射需要实测，不对目录开放其他用户权限。
3. 在原 Compose 参数后增加 `-f compose.danmaku.yaml`。仅使用 YouTube 单向 Webhook 时同时保留 `compose.youtube-webhook.yaml`；独立回复群不使用该覆盖文件。此覆盖文件只挂载队列目录并启用弹幕配置，没有入站端口。
4. 在宿主登录用户下定期执行以下命令（例如 macOS LaunchAgent `StartInterval=20`）；一次只处理一个分析任务，进程锁防止并发。Codex 可执行文件用实际安装路径，不使用 shell 字符串拼接。

```sh
/部署目录/bin/shadowbrain-comment-bridge review-danmaku \
  --queue-dir /部署目录/danmaku-queue \
  --codex-bin /Codex实际安装路径/codex
```

工作命令不读取 `runtime.env`，无需飞书、B 站或 YouTube 凭据；直接继承本机用户的 Codex 登录。模型固定 `gpt-6.1-sol` / `medium`，单次分析最多 4 分钟。退出、断电或杀进程遗留的分析 claim 在下次执行时标为失败，不自动重放。LaunchAgent 的 stdout/stderr 只记录安全错误类别，禁止输出队列正文。宿主用户未登录、机器休眠或 Codex 配额耗尽会影响分析及时性。

首次启用增加 `danmaku_sources`、`danmaku_seen`、`danmaku_jobs` 三个表，不改变评论表或已有消息映射。0.1.16 另增 `danmaku_alert_keys` 保存异常内容去重记录，升级后产生的提醒按视频、原文及毫秒时间点合并。升级前按下节备份 SQLite 与配套凭据，并保存 Compose 配置。回滚可停止宿主工作器、去掉弹幕 Compose 覆盖文件并恢复旧镜像；旧版本忽略新增表，应保留当前数据库及凭据，避免回放切换后的外部动作。保留队列用于审计，不通过删除数据库或重置 `unknown` 排除故障。

0.1.17 增加 `bilibili_risk_stop`，全局阻止风控后的 B 站请求，并保存飞书告警状态。`status` 中 `state=stopped` 表示需人工确认；运行期间 `inflight` 是正在执行一个请求，异常中断后重启会转为停用。不要删除该表、恢复旧库或换凭据解除停用。回滚到 0.1.16 及以前版本会失去此保护：若已停用或请求结果不确定，必须保持 B 站禁用（`SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED=false`）或保持服务停止，不能直接启动旧版本恢复 B 站请求。

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
