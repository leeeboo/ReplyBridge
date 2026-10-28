# 评论桥接 Docker 部署

此部署只把 `shadowbrain-comment-bridge` 二进制、运行依赖和许可证放进镜像。SQLite 数据库、飞书密钥、YouTube OAuth 文件和 B 站 Cookie 始终由目标主机外挂，绝不会进入构建上下文或镜像。

## 首次部署

需要 Docker Engine / Docker Desktop 和 Docker Compose v2（支持 `env_file.required`，至少 2.24.0）。先按[首次安装指南](../../docs/quick-start.md)准备飞书及所选平台凭据；授权在桌面电脑完成。仅使用一个平台时，另一个保持禁用，不必创建它的授权文件。

Docker 镜像从源码构建，Release 二进制包不含 Go 源码。选择 Docker 部署时先克隆公开仓库并检出目标版本（见首次安装指南）；在源码仓库根目录构建镜像，默认匹配当前 Docker 主机架构：

```sh
docker build --file deploy/comment-bridge/Dockerfile --tag shadowbrain-comment-bridge:0.1.22 .
docker run --rm --network none shadowbrain-comment-bridge:0.1.22 version
```

给另一种架构部署时用 `docker buildx build --platform linux/amd64` 或 `linux/arm64`，并添加 `--load`；不能把 ARM64 镜像直接当作 x86 原生制品。Dockerfile 固定 Go 安全补丁及基础镜像 digest。构建上下文忽略凭据、dotenv、数据库、备份和历史发布包，最终镜像只带程序与许可证。

下列示例只适用于全新部署目录和全新数据卷；已有部署不要重做初始化，也不要复制 `.env.example` 覆盖运行配置。部署目录与源码目录分开，拉取源码不会修改运行配置或自动重启服务。

```sh
umask 077
mkdir ../replybridge-deploy
cp deploy/comment-bridge/compose*.yaml deploy/comment-bridge/compose.env.example ../replybridge-deploy/
cp .env.local ../replybridge-deploy/runtime.env
cp -R secrets bilibili-auth ../replybridge-deploy/
cd ../replybridge-deploy
cp compose.env.example compose.env
mkdir -p -m 700 staging backups
chmod 700 . secrets bilibili-auth staging backups
chmod 600 compose.env runtime.env
find secrets bilibili-auth -type f -exec chmod 600 {} +
```

仅在部署目录的 `secrets/` 放本服务所需文件；整个目录只读挂入 `/run/shadowbrain/secrets`，未启用平台的文件可不存在。B站 `bilibili-auth/` 单独可写挂载，供扫码成功后原子替换凭据。即使只启用 YouTube，也保留空的 `bilibili-auth/` 目录。Compose 保留原有文件路径、服务名和外部数据卷名；旧的双平台配置无需迁移数据库。

```text
replybridge-deploy/
├── compose.yaml / compose.env / runtime.env
├── secrets/                   # 0700，仅放本服务凭据
│   ├── feishu-app-secret       # 必需，0600
│   ├── youtube-client.json    # 启用 YouTube 时必需，0600
│   └── youtube-token.json     # 启用 YouTube 时必需，0600
├── bilibili-auth/              # 0700
│   └── credential.json        # 启用 B站时必需，0600
├── staging/                   # 0700
└── backups/                   # 0700
```

### Linux 文件所有权

默认运行用户是 `10001:10001`。原生 Linux 的 bind mount 保留宿主 UID；仅把本次新部署的凭据目录交给该用户，不能通过开放到 `0644/0777` 解决：

```sh
sudo chown -R 10001:10001 secrets bilibili-auth
```

Docker Desktop 的文件共享映射不同，先做下面的实际读取检查，不盲目更改宿主用户。若自定义 UID/GID，还需由管理员初始化数据卷所有权；新手建议保留默认值。

### 配置、数据卷和启动

`runtime.env` 使用安装时的配置；Compose 自动覆盖 DB 和凭据路径为容器路径。`SHADOWBRAIN_YOUTUBE_ENABLED` 和 B站双开关仍决定启用的平台。不要使用 Compose `secrets:` 的默认 `0444` 权限。

确认 `compose.env` 中的数据卷名未被其他实例使用，然后在新主机创建：

```sh
docker volume create shadowbrain-comment-bridge-data-v1
docker compose --env-file compose.env -f compose.yaml config --quiet
docker compose --env-file compose.env -f compose.yaml run --rm --no-deps --entrypoint /bin/sh comment-bridge -ec '
  test -r /run/shadowbrain/secrets/feishu-app-secret
  if [ "$SHADOWBRAIN_YOUTUBE_ENABLED" = true ]; then
    test -r /run/shadowbrain/secrets/youtube-client.json
    test -r /run/shadowbrain/secrets/youtube-token.json
  fi
  if [ "$SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED" = true ]; then
    test -r /run/shadowbrain/bilibili-auth/credential.json
    test -w /run/shadowbrain/bilibili-auth
  fi
'
docker compose --env-file compose.env -f compose.yaml up -d
docker compose --env-file compose.env -f compose.yaml ps --all
```

`data-init` 在空卷首次挂载时保留镜像中目录的 `10001:10001` 所有权，并限制目录/文件权限。数据卷是 external volume，`compose down -v` 不删除它；不要手工删除数据卷或重置任务。

服务没有入站端口，不配置 `ports`。健康检查仅验证数据库可读；飞书、平台轮询及人工引用回复还需按[安装指南](../../docs/quick-start.md#4-启动与验收)验收。首次基线期间飞书连接可能延迟数分钟。HTTP 写请求超时保留 `unknown`，不自动重试。

B站运行中的 `-101/-111` 登录失效可通过原群二维码刷新，要求飞书 `im:resource` 权限。文件缺失、权限或格式错误导致启动失败，应先修复本地配置；风控停用不能通过重新扫码解除。详细行为见[使用文档](../../docs/comment-bridge.md#cookie-失效告警)。

## YouTube 独立回复群

0.1.18 起，在 `runtime.env` 设置 `SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID` 可使用同一应用机器人向另一个群发送 YouTube 通知并接收引用回复。机器人必须已加入该群；保留原 `SHADOWBRAIN_FEISHU_CHAT_ID`，B站和告警仍走原群。使用此模式时清空 `SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE`，Compose 参数不再包含 `compose.youtube-webhook.yaml`。不迁移或重置数据库，旧 Webhook 消息不补发。回滚到 0.1.17 时恢复原 Webhook 配置及 Compose 参数，保留当前数据库和 B站风控状态。

0.1.19 首次启动会在事务中移除 `feishu_messages` 的评论级唯一约束，保留消息 ID 唯一性、全部旧行及外键关系，用于人工授权重发后的多消息映射。上线前备份并在副本验证迁移；正常通知去重仍由 `notification_jobs` 保证。0.1.18 可读取迁移后的库，回滚保留当前库及新消息映射，不恢复旧快照丢弃已发送动作。

## YouTube 独立 Webhook

将完整 URL 保存在 `secrets/youtube-feishu-webhook`，由部署用户拥有且权限为 `0600`。该文件随 `secrets/` 目录只读挂载；可选 Compose 覆盖文件设置单向通知路径，不进入镜像。未启用分流的部署不需要这个文件。

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

1. 在旧实例仍运行时预建目标目录、加载镜像并验证飞书及所启用平台的凭据的容器内视图均为普通 `0600` 文件；`bilibili-auth` 目录为 `0700` 且容器用户可写。
2. 停止旧实例并确认进程退出。
3. 用 SQLite `.backup` 生成最终一致性快照；校验 `PRAGMA integrity_check`、`foreign_key_check`、source/comment/outbox 聚合。
4. 用同一镜像、`network=none` 和非 root UID/GID，把快照作为 `/var/lib/shadowbrain/comment-bridge.db` 导入 named volume 并设为 `0600`。不要复制旧 `-wal`/`-shm`。同时把旧主机的 `bilibili-auth/` 目录（目录 `0700`，凭据 `0600`）复制到新主机；凭据目录必须独立备份，不能放入 SQLite 快照或镜像。
5. 启动容器，等待 `healthy`，确认版本、数据库聚合、飞书长连接及一轮平台轮询。
6. 完成真实的“新评论 → 飞书通知 → 引用回复 → 平台回复”测试，并分别验证 B 站 👍 与 YouTube 👍 手动提示。

### 安全回滚

`unknown` 动作不得自动重试。升级失败时先停止新实例和弹幕工作器，保存**切换后的数据库一致性快照、配套 B站凭据及配置**，并核对切换后已发送、正在发送和不确定的外部动作。

- 0.1.22 未修改数据库结构，回滚至 0.1.21 时保留当前数据库和配套凭据，只切回旧镜像；这样不会丢失新动作的去重记录。
- 只有已确认切换后没有新增外部动作、没有丢失待处理或不确定结果的风险，且旧状态与平台一致时，才可恢复切换前数据库及对应凭据。旧快照无法撤销已发布的回复或已删除的评论。
- 已产生任何新动作或无法确认时，禁止直接覆盖为旧库。先保留证据、核对平台并确定不会重放的恢复方式；必要时保持停止。
- 切换前后备份都包含 SQLite 快照、B站凭据和配置；不复制运行中 WAL/SHM，不删除风控停用记录，始终只有一个实例。

0.1.22 将静态凭据改为目录只读挂载；旧 Compose 的单文件挂载仍可用于已有双平台部署，回滚无需改变凭据路径。单平台新部署应保留新的目录挂载，避免重新引入未启用平台文件的强制依赖。

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
