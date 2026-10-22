# 飞书评论桥接部署说明

`shadowbrain-comment-bridge` 默认把新评论通知到指定飞书应用机器人目标群（可含外部成员），并仅在人工明确引用该机器人原消息后发布回复。YouTube 可单独改用自定义 Webhook 机器人，只推送通知。通知统一使用 `💬 新评论`，不再附平台标题或平台字段；B站响应明确标记充电会员时，作者行追加 `⚡充电会员`。它不是自动回复器：不满足群、人员、消息文本和映射条件的消息一律忽略。

## 边界与前提

- 仅运行一个实例。桥接使用单实例 SQLite 和 runlock；同一数据库不得同时启动两个 `run`。
- 首次运行只建立当前评论基线，**不补发历史评论**。
- YouTube 使用官方 [YouTube Data API `commentThreads.list`](https://developers.google.com/youtube/v3/docs/commentThreads/list) 轮询，并用官方 [`comments.insert`](https://developers.google.com/youtube/v3/docs/comments/insert) 回复。官方 [Comments resource](https://developers.google.com/youtube/v3/docs/comments) 没有评论点赞写接口：对 YouTube 通知添加飞书 👍 时，服务不会调用 YouTube API，而会由机器人引用原通知回复一次，提示打开评论直达链接手动点赞。
- ChatFuse 仅覆盖 B 站 / YouTube 直播聊天读取，不能用于普通视频评论写入；其 `youtube.readonly` refresh token 不能复用。
- B 站实验功能调用非官方 Web 接口（参考相邻 `bilibili-comment-studio` 的 `/x/v2/reply/up/fulllist`、`/x/v2/reply/add` 与评论点赞接口）。它不是官方、稳定或已确认合规的接口；默认关闭，使用前自行确认账号、条款和风险。点赞会使用当前 Cookie 对对应评论发起不可撤销的外部写入。
- 不要把 app secret、Google client JSON、refresh token 或 B 站 Cookie 放入 dotenv、Git、聊天记录或命令行参数。二维码会公开发送到配置的群，这是本项目明确选择的群内协作方式；不要把二维码转发到群外。

## 构建与文件权限

```bash
go build -o shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge
mkdir -p -m 700 ./secrets
touch ./secrets/feishu-app-secret
chmod 600 ./secrets/feishu-app-secret
# 用本机受控编辑器把飞书 App Secret 写入上面的文件；不要粘到 shell 命令中。
```

`SHADOWBRAIN_FEISHU_APP_SECRET_FILE`、`SHADOWBRAIN_YOUTUBE_CLIENT_FILE`、`SHADOWBRAIN_YOUTUBE_TOKEN_FILE` 和 `SHADOWBRAIN_BILIBILI_CREDENTIAL_FILE` 都应指向当前用户拥有的普通文件；secret、client JSON、token、Cookie 文件均设为 `0600`。例如：

```bash
chmod 600 ./secrets/feishu-app-secret ./secrets/youtube-client.json ./secrets/youtube-token.json
chmod 600 "$HOME/.bili-sync/credential.json"
```

## 飞书内部应用机器人

B站及需要双向引用回复的通知必须使用**自建内部应用的应用机器人**。YouTube 单向分流使用下节的自定义 Webhook，不影响原应用机器人。

1. 在飞书开放平台创建自建应用，添加并启用“机器人”能力；记录 App ID。App Secret 仅写入上节的 `0600` 文件。
2. 在“事件订阅”选择**长连接**，订阅 `im.message.receive_v1` 和 `im.message.reaction.created_v1`（[接收消息文档](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive?lang=zh-CN)、[表情新增事件文档](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/event/created)；[长连接配置](https://open.feishu.cn/document/server-docs/event-subscription-guide/event-subscription-configure-/request-url-configuration-case?lang=zh-CN)）。
3. 申请最小发送权限 `im:message:send_as_bot`、添加状态表情所需的 `im:message.reactions:write_only`，接收 👍 事件所需的 `im:message.reactions:read`，以及上传 B 站登录二维码所需的“获取与上传图片或文件资源”权限 `im:resource`（[权限说明](https://open.feishu.cn/document/server-docs/application-scope/scope-list?lang=zh-CN)）。新增 `im:resource` 后必须重新发布应用，旧版本不会获得上传权限。应用机器人支持加入目标外部群；仍须将精确 `chat_id` 配入环境文件。
4. 为了让授权人员可直接“引用回复”而无需额外 `@`，还必须申请敏感权限 `im:message.group_msg`（接收群内全部用户消息）。
   - 若只申请 `im:message.group_at_msg:readonly`，则操作方式改为：**引用原机器人消息并 @机器人**，否则事件不会送达。
5. 发布/启用应用后，将机器人加入目标群（可含外部成员）；把该群的精确 `chat_id` 及授权模式配入环境文件。

首次配置时，可在应用发布并加入测试群后，用下列命令发现测试群和当前发言人的标识。命令只输出 `chat_id/open_id`，不读取或保存消息正文；运行后在目标群发送任意测试文本，得到一行标识后命令会自动退出。

```bash
./shadowbrain-comment-bridge discover-feishu \
  --app-id YOUR_APP_ID \
  --secret-file ./secrets/feishu-app-secret
```

授权模式必须二选一：默认安全模式为 `SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS=false`，并填写逗号分隔、不可变的 `SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS`；或显式设置 `SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS=true` 且将 `ALLOWED_OPEN_IDS` 留空。后者会让精确 `chat_id` 内所有 `sender_type=user` 且 `open_id` 非空的真人（包括外部联系人）获得公开平台回帖权，群成员变动会立即改变授权范围。外部单聊不需要、也不应开启该开关。

桥接仅接收同时满足以下条件的回复事件：目标 `chat_id` 一致、发送者符合所选授权模式、消息有用户文本，并且**直接引用的 parent 消息**能映射回本桥接此前发送的机器人通知。只处于同一话题、引用话题内其他人工消息或仅携带 root 的消息不会触发平台回复；其余未直接引用本桥机器人通知、非目标群、未授权人员和机器人消息也都会忽略。

对 B 站通知，授权人员在**机器人原通知**上新增飞书 `THUMBSUP`（显示为 👍）可请求点赞该评论。即使启用全群授权，内部和外部联系人也只能触发本服务登记、且属于精确 `chat_id` 的通知；其他消息的表情一律忽略。移除 👍 不会撤销已发出的 B 站点赞。对原应用机器人发送的 YouTube 通知添加 👍 不会调用 YouTube API；机器人会引用该原通知发送一次手动点赞提示。该提示按通知去重，重复事件或多人添加 👍 都不会额外刷屏，复用现有机器人发送权限与事件订阅，不增加任何权限或事件。

## 弹幕巡检（0.1.16）

可选设置 `SHADOWBRAIN_DANMAKU_QUEUE_DIR` 为现有、权限 `0700` 的绝对目录，并在已登录 Codex 的宿主机定期执行 `review-danmaku` 工作命令。默认留空，不影响原有评论功能。

- 每 10 分钟读取一次 B 站创作者后台最近弹幕，一次请求最多 100 条，不按单个视频逐个读取。使用已有 B 站凭据、请求间隔和冷却状态；遇到风控立即停止，不额外重试。
- 首次成功读取只建立基线；之后按最新顺序处理，遇到已读弹幕即停止。已读 ID、下次读取时间和分析任务保存在 SQLite，重启不重复推送。两次读取之间超过 100 条时，只覆盖最近 100 条，不补抓更早弹幕。
- 有新增才调用本机官方 `codex exec`，固定 `gpt-6.1-sol`、`medium`。只判断视频问题反馈、疑似引战和恶意信息；仅由观众自身网络、设备或播放器设置造成的卡顿、缓冲、无法观看高清不提醒，明确指出的独立视频问题仍保留。正常批次保持安静。弹幕作为不可信数据传入，禁用工具和插件，不读取评论凭据，不执行弹幕中的指令。
- 异常结果发到原 B 站飞书应用机器人所在群，不 @；附视频定位链接、原文和原因。同一视频、原文完全相同且毫秒时间点一致的异常合并为一条；去重记录持久化，后续批次及重启也不重复提醒，发送结果不确定时仍保留记录。每批合并一条消息，展示前 10 条合并后的异常及总数，完整结果保存在本机数据库。AI 判断需要人工复核，不自动删除、举报或回帖。
- 分析在宿主机执行，容器继续处理评论；队列仅保存最小弹幕数据，使用 `0600` 文件，消费后清理。Codex 直接管理已有登录，认证文件不复制进容器。需要遵守账户额度；本方案用于用户自己的可信本机任务，不能将个人登录开放为商业或托管 AI 服务。参见 [Codex 非交互模式](https://learn.chatgpt.com/docs/non-interactive-mode#authenticate-in-automation)。
- 分析失败会记录终态并通过现有错误告警提示，不把失败当作“无异常”；工作器超过 30 分钟未返回会告警。通知发送结果不确定时保留 `unknown`，不会自动重发。`status` 可查看 `danmaku` 任务数量；`succeeded` 包含无需通知的正常批次。

部署、宿主机工作器和回滚见[部署文档](../deploy/comment-bridge/README.md#弹幕分析工作器)。

## YouTube 独立回复群（0.1.18）

将同一个应用机器人加入 YouTube 目标群，并设置 `SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID` 为该群精确 `chat_id`，即可让后续 YouTube 通知单独发送到这个群，支持直接引用原通知回复。B站、弹幕和运行告警继续发送到 `SHADOWBRAIN_FEISHU_CHAT_ID`。两个群沿用同一人员授权模式；每条回复还必须匹配原通知保存的群 ID，不能跨群引用绕过。外部群需满足应用的对外共享设置及飞书的入群限制。

此模式与 `SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE` 互斥；切换时清空 Webhook 配置并移除 `compose.youtube-webhook.yaml` 覆盖文件。已有通知去重和消息映射保留，不补发历史评论。旧 Webhook 消息无法追溯建立映射，引用它们仍不会发布回复。两项均留空时保持原来的单应用群模式。

0.1.19 起允许同一评论保存多条应用机器人通知映射，供明确授权的人工重发验证使用；新旧通知各自校验其目标群，旧映射不被替换。正常轮询仍按评论去重，回复、点赞和未知结果不自动重放。

## YouTube 独立 Webhook 通知（0.1.14）

把[飞书自定义机器人](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)的完整 URL 放入当前用户拥有的普通 `0600` 文件，并设置 `SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE` 指向它。URL 属于凭据，不放入 dotenv、日志或仓库；只接受 `https://open.feishu.cn/open-apis/bot/v2/hook/...`。留空时沿用原应用机器人；配置文件缺失、权限不符或 URL 无效时拒绝启动，避免误发到原群。

启用后，只有新产生的 YouTube 评论通知改发该机器人，文案提示打开评论链接回复或点赞。Webhook 不返回消息 ID，也不接收引用回复或表情事件；因此仅登记通知已送达，不创建双向回复映射。历史应用机器人通知的映射保留。首次基线、通知去重和 `unknown` 不自动重试规则不变，不补发或重发历史通知。B站通知、引用回复、点赞、登录二维码及运行错误告警仍使用原应用机器人。

Docker 部署使用可选的 `compose.youtube-webhook.yaml`，详见[部署文档](../deploy/comment-bridge/README.md#youtube-独立-webhook)。

## YouTube：官方 Data API 与 OAuth

YouTube 的官方 [Comments resource](https://developers.google.com/youtube/v3/docs/comments) 没有评论点赞写方法。因此，飞书通知中的“点赞”仅指手动动作：打开评论直达链接，在 YouTube 上自行点赞；服务不会把飞书 👍 持久化为 YouTube 点赞任务，也不会添加成功 `OK` 表情。通过原应用机器人发送的 YouTube 通知首次收到有效 👍 后，机器人会引用原通知提示这项手动操作；它不调用 YouTube API，重复或多人 👍 不会重复提示。

1. 在 [Google Cloud Console](https://console.cloud.google.com/) 创建或选择项目，启用 **YouTube Data API v3**。
2. 按 [Google OAuth 2.0 已安装应用流程](https://developers.google.com/identity/protocols/oauth2/native-app) 配置 OAuth consent screen，并在“凭据”创建 **Desktop app（已安装应用）** OAuth client；下载 client JSON 到仅自己可读的位置并执行 `chmod 600`。不要创建 Web client：此命令使用本机 loopback desktop 流程。
3. 授权账号必须有目标频道的评论回复权限；运行：

   ```bash
   ./shadowbrain-comment-bridge auth youtube \
     --client-file ./secrets/youtube-client.json \
     --token-file ./secrets/youtube-token.json
   chmod 600 ./secrets/youtube-token.json
   ```

   浏览器完成新授权后，token 文件必须包含 `https://www.googleapis.com/auth/youtube.force-ssl`；不能复用 ChatFuse 或其他仅有 `youtube.readonly` 的 token。
4. 配置频道 ID、client JSON、token 文件和轮询页数；账号 ID 可留空，默认使用频道 ID。轮询读取使用 [`commentThreads.list`](https://developers.google.com/youtube/v3/docs/commentThreads/list)，回帖使用 [`comments.insert`](https://developers.google.com/youtube/v3/docs/comments/insert)。

桥接同时监控顶级评论和评论下的用户回复；当 `commentThreads.list` 只返回部分回复时，会按需用官方 [`comments.list`](https://developers.google.com/youtube/v3/docs/comments/list) 补齐。机器人自己发布的频道回复会被过滤，避免回显通知。
启动时还会用官方 `channels.list(mine=true)` 校验 OAuth 身份确实拥有配置的频道；不匹配会拒绝启动，避免把回复误发到别人的视频。

`SHADOWBRAIN_YOUTUBE_MAX_PAGES` 是每轮覆盖的最新顶级评论线程窗口（每页最多 100 条）；子回复监控也只覆盖这个窗口。值越大越能覆盖旧线程里的新回复，但按 2 分钟轮询时每增加 1 页约增加 720 quota units/天，务必结合项目配额选择。

注意项目配额：默认配额和每个方法的成本以 Google 的 [配额说明](https://developers.google.com/youtube/v3/determine_quota_cost) 为准；轮询频率、`MAX_PAGES` 和人工回复量都消耗配额，先在小值下观察 Google Cloud 的用量。

## 异常告警（0.1.13）

机器人将抓取、存储、消息处理、回复、点赞、状态表情、登录与二维码错误合并通知到原目标群。每 30 秒检查待发告警及落库的 failed/unknown 任务；持续同类错误只报告一次，恢复后再次发生或新增失败任务最多每类 6 小时一条，全局至少间隔 10 分钟。首次上线会汇总此前尚未报告的失败任务；不会重新执行这些任务。告警类别和去重状态保存在 SQLite，重启不重复发送。发送失败或结果不确定的告警按 6 小时限频重试，不递归报警。仅发送固定类别，不发送上游错误正文、评论正文或密钥。

进程停止、主机离线、飞书发送权限或网络不可用时，机器人无法即时通知；数据库不可用时退化为进程内每 6 小时至多一次的存储故障提示，无法保证跨重启去重。该功能不是独立的服务器存活监控。

## B 站：实验性双门开关

创作中心列表可能仍包含已删除的评论；子回复接口明确返回 `12006` 时，保留已获得的评论并继续其他线程，回复数量变化前不重复查询该失效线程，避免单条评论阻塞整轮推送。

自 0.1.17 起，所有 B 站 HTTP 请求共用持久化风控拦截：HTTP `403/412/429`，API `-352/-403/-412/412/-509/-102` 会立即停止后续评论、子回复、弹幕、Nav、回复、点赞及二维码登录请求，并立即尝试向飞书发送一条明确停用告警，不受普通错误告警的冷却限制。`-403` 按保守风控处理，不再刷新签名重试。停用状态写入 SQLite 的 `bilibili_risk_stop`，到期、重启、换 Cookie 或扫码都不能解除；必须人工检查后明确授权恢复。请求期间进程异常退出、无法确认结果时，重启同样保持停用。请求前无法写入保护状态则不发请求。`status` 可查看停用及告警投递状态；飞书失败或结果不确定时记录 `failed/unknown`，不自动重发，但 B 站始终停用。YouTube 和飞书事件服务继续运行。

普通网络或其他读取错误仍暂停 B 站评论/弹幕读取至少 1 小时，连续失败按 1、2、4、8、16、24 小时退避；到期仅恢复读取验证，冷却及验证期间禁止回复和点赞。登录失效码 `-101/-111` 单独走凭据恢复流程，不能解除已经触发的风控停用。请求间 2–6 秒节流和定期休息保持不变。普通故障冷却仍在进程内维护，不得反复重启绕过；非官方接口始终存在账号风险。

已有服务器继续使用外挂 Cookie，无需重新授权。首次配置可使用已安装的独立 `bili-sync` 授权工具取得 Cookie（默认路径通常为 `$HOME/.bili-sync/credential.json`）；该工具不从本项目或父项目构建：

```bash
bili-sync auth qr
chmod 600 "$HOME/.bili-sync/credential.json"
```

然后才可同时设置下列两个值，缺一不可：

```dotenv
SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED=true
SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK=I_UNDERSTAND_BILIBILI_WEB_API_ACCOUNT_RISK
```

这是账号风险确认，不代表接口获得官方支持、稳定性保证或合规结论。要停用，设 `ENABLED=false` 并清空 ACK；不要删除仍可能被其他 `bili-sync` 命令使用的凭据文件。

### Cookie 失效告警

运行中的 B 站接口明确返回未登录码 `-101` 或 CSRF 失效码 `-111` 时，桥接会向配置的飞书群发送固定告警，并可在群内发送登录二维码；这两个码以外的风控、限流或普通网络错误不会被误判为 Cookie 失效。相同账号在同一失效事故中只发送一次。风控停用期间不会生成或轮询二维码。二维码有效期 180 秒，扫码完成后会强校验登录账号 ID 必须与原配置账号一致，再以原子方式更新外挂凭据并恢复监控。二维码公开在目标群是本功能明确选择，不应转发群外。二维码过期后，直接引用原二维码消息并回复 `重新登录`，机器人会重新生成并发送二维码；该文本沿用现有引用回复事件入口。应用必须有 `im:resource` 且在权限变更后重新发布，否则无法发送二维码。`-101` 可由后续成功轮询重新布防；读取接口不能证明 CSRF 可写，因此 `-111` 必须等一次成功回复或点赞后才重新布防。告警、二维码流程均不记录 Cookie、二维码原文或上游响应到日志。

手工替换外挂凭据后必须重启服务，当前进程不会监视文件变化；群内二维码登录成功则由服务自行原子更新并热加载，无需手工重启。启动阶段凭据文件不存在、权限不符合 `0600` 或格式损坏仍会直接启动失败；这类本地配置错误不会伪装成运行时失效告警。

## `.env.local` 完整桥接模板

复制以下内容到未提交的 `.env.local`，再填写本机路径和标识。所有 secret 值保持在对应 `0600` 文件中而非这里。

```dotenv
# Bridge SQLite / scheduling
SHADOWBRAIN_COMMENT_BRIDGE_DB=comment-bridge.db
SHADOWBRAIN_COMMENT_POLL_INTERVAL=2m
SHADOWBRAIN_COMMENT_WORK_INTERVAL=1s

# Feishu internal app bot; the target group may include external members.
SHADOWBRAIN_FEISHU_APP_ID=
SHADOWBRAIN_FEISHU_APP_SECRET_FILE=./secrets/feishu-app-secret
SHADOWBRAIN_FEISHU_CHAT_ID=
# Safe default: comma-separated immutable open_id values, no display names.
# Leave empty only when enabling the explicit group-wide authorization below.
SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS=
# false by default. true authorizes all human members (including external
# contacts) of this exact chat_id; ALLOWED_OPEN_IDS must then stay empty.
SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS=false

# YouTube Data API v3 (official)
SHADOWBRAIN_YOUTUBE_ENABLED=false
SHADOWBRAIN_YOUTUBE_CHANNEL_ID=
SHADOWBRAIN_YOUTUBE_ACCOUNT_ID=
SHADOWBRAIN_YOUTUBE_CLIENT_FILE=./secrets/youtube-client.json
SHADOWBRAIN_YOUTUBE_TOKEN_FILE=./secrets/youtube-token.json
SHADOWBRAIN_YOUTUBE_MAX_PAGES=2
# 可选：独立的应用机器人回复群；与下面的 Webhook 互斥。
SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID=
# 可选：完整 Webhook URL 放入独立 0600 文件；留空沿用应用机器人。
SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE=

# Bilibili video comments (experimental, unofficial Web API; default off)
SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED=false
# Must be empty unless deliberately enabling; exact required literal:
# I_UNDERSTAND_BILIBILI_WEB_API_ACCOUNT_RISK
SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK=
# 留空时使用当前账号的默认路径 ~/.bili-sync/credential.json；dotenv 不展开 $HOME。
SHADOWBRAIN_BILIBILI_CREDENTIAL_FILE=
# 可留空；默认从 Cookie 取得 DedeUserID；若填写则必须与 Cookie 一致。
SHADOWBRAIN_BILIBILI_ACCOUNT_ID=
# 单个有变化评论线程的子回复分页上限：1-50，默认 10。
SHADOWBRAIN_BILIBILI_MAX_SUB_REPLY_PAGES=10
```

评论轮询按日本时间自动降频：每天 21:00（含）至次日 03:30（不含）固定每 20 分钟一次，其余时段使用 `SHADOWBRAIN_COMMENT_POLL_INTERVAL`。到达 21:00 或 03:30 边界时会立即切换频率；飞书消息、回复、点赞和凭据恢复任务仍按 `SHADOWBRAIN_COMMENT_WORK_INTERVAL` 处理，不受夜间降频影响。

`.env.example` 只提供变量名和安全默认值；不要以为把真正的 token/Cookie 写进 `.env.local` 就安全——它们仍应留在上述独立 `0600` 文件。确认 `.env.local` 和 `secrets/` 已被忽略后再运行 `git status`。

## 首次运行、人工回复与故障处理

首次启动会写入当前评论基线而不推送旧评论：

```bash
./shadowbrain-comment-bridge run
```

之后桥接向飞书群发送新评论通知。人工回复时，在飞书中**引用该机器人通知消息**，直接发送要发布的用户文本；桥接只从引用关系解析目标，不接受用户提供的平台评论 ID。若你的飞书应用只授予 `im:message.group_at_msg:readonly`，请在这条引用回复中同时 `@机器人`。

桥接持久化这条引用回复后，会尝试在该人工消息上添加 `StatusReading`（已受理/已读）表情；仅在平台回帖确认成功后，才会尝试添加 `OK` 表情。B 站点赞也是独立持久化任务：平台明确成功后，机器人会在**原评论通知**上添加 `OK`；移除 👍 不会撤销 B 站点赞。YouTube 没有自动点赞任务，因此不会因 👍 添加 `OK`，而是在原通知下发送一次手动点赞提示。该提示也是独立持久化动作；失败或 `unknown` 不会改变回帖/点赞任务终态。

当 B 站 Cookie 失效时，群内二维码由机器人直接发送，二维码有效期为 180 秒。扫码后服务会校验返回的 B 站账号 ID 与当前账号一致；不一致会拒绝写入。二维码消息过期后，在该消息上直接引用并回复 `重新登录`，即可重新生成二维码。此流程需要应用身份权限 `im:resource`，权限新增或变更后必须重新发布应用。二维码公开发送到目标群是有意设计；服务不会把 Cookie、二维码内容或上游响应写入日志。状态命令会列出 `credential-refreshes` 二维码刷新任务。

B 站同时解析顶级评论和子回复；管理列表中的子回复不完整且回复数发生变化时，会额外分页补齐。`SHADOWBRAIN_BILIBILI_MAX_SUB_REPLY_PAGES` 默认 10、允许 1–50；顶级列表每轮从最新评论向后翻页，遇到上轮已见评论即停止，单轮最多检查 5 页（250 条）。这些都是对非官方接口的保守风控上限；超过所配子回复页数的更深回复不会被本轮覆盖。若两次轮询或停机期间新增超过顶级列表上限，较早评论可能漏收，应缩短轮询间隔并在受控账号上观察风控情况。

查看状态：

```bash
./shadowbrain-comment-bridge status
```

`status` 会列出待处理、发送中、已发送、失败和 `unknown` 的通知、引用回复、B 站点赞动作、B 站 Cookie 失效告警，以及名为 `manual-engagement-feedbacks` 的 YouTube 手动点赞提示 outbox。网络超时等结果不确定的外部写入会标为 `unknown`，因为它**可能已经发布成功**。`unknown` 绝不自动重试：先人工核对（提示需核对飞书原通知下是否已有机器人回复）。由于 `run` 独占运行锁，先停止桥接进程；然后只对一个已核对、确认未执行的 action ID 执行：

```bash
./shadowbrain-comment-bridge retry-unknown \
  --action ACTION_ID \
  --confirm-possible-duplicate
```

此命令只把该动作恢复为 `pending`，不会在命令内发起外部请求；执行后重新启动 `run` 才会处理。它不会批量重试；即使人工核对后仍有竞态，重试也可能造成重复发布、重复点赞或重复提示。不要对未知结果做自动或循环重试。

可用 launchd（macOS）或 systemd（Linux）把同一 `run` 命令作为单一常驻进程启动，并在服务定义中设置工作目录、受限用户和 `.env.local` 路径。macOS arm64 的非 root Docker/Compose 部署、外挂文件、named volume、迁移与回滚步骤见 [Docker 部署说明](../deploy/comment-bridge/README.md)。更新或迁移前必须先停掉旧进程，避免与 runlock/SQLite 单实例约束冲突。

## 验证

```bash
go test ./...
go build -o shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge
./shadowbrain-comment-bridge help
# 首次 run 已创建数据库后：
./shadowbrain-comment-bridge status
```

在启用任一写入平台前，先核对 `.env.local` 中的目标群、频道/账号；用受控测试评论验证飞书通知、引用映射和一次人工回复路径。
