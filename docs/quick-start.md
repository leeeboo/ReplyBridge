# 首次安装 ReplyBridge

仅在新的工作目录执行本页。已有部署升级前先阅读[数据切换与回滚](../deploy/comment-bridge/README.md#数据导入与切换)，不要覆盖正在运行的配置、凭据或数据库。

## 1. 下载或构建

支持 Linux amd64/arm64、macOS Intel/Apple Silicon。Windows 使用 WSL2 或 Linux 容器；原生 Windows 缺少本项目要求的进程锁。

在 [Releases](https://github.com/leeeboo/ReplyBridge/releases) 下载对应的 `replybridge_版本_系统_架构.tar.gz` 及 `SHA256SUMS`。Linux 用 `sha256sum -c SHA256SUMS --ignore-missing`；macOS 用 `shasum -a 256 下载的压缩包`，与清单中同名文件比较，然后解压到新目录。macOS 二进制没有签名或公证；不修改系统安全设置，受系统策略阻止时改用源码构建。

源码构建需要 Git、Go 1.26.10 或兼容的新安全补丁版本：

```sh
git clone https://github.com/leeeboo/ReplyBridge.git
cd ReplyBridge
git checkout v0.1.22
go test ./...
go build -trimpath -o shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge
./shadowbrain-comment-bridge version
```

以下命令均在源码根目录或二进制解压目录执行。直接运行和 Docker 部署二选一，禁止共用数据库同时启动。

## 2. 准备飞书机器人和私密目录

```sh
umask 077
mkdir -p -m 700 secrets bilibili-auth
cp -n .env.example .env.local
touch secrets/feishu-app-secret
chmod 600 .env.local secrets/feishu-app-secret
```

在飞书开放平台创建自建应用，启用机器人和长连接，订阅消息与新增表情事件，授权发送消息、读取/写入表情及上传资源。接收群消息的两种权限模式、应用发布和标识发现命令见[飞书配置](comment-bridge.md#飞书内部应用机器人)。

用受控编辑器把 App Secret 写入 `secrets/feishu-app-secret`，不要写入命令行或 dotenv。编辑 `.env.local` 中的 App ID、目标 chat ID，并选择人员白名单或显式的全群授权；两种授权模式不能混用。机器人必须已加入目标群。

## 3. 至少配置一个平台

### B站

B站使用非官方 Web 接口，默认禁用；启用前确认账号和平台风险。首次授权使用公开的 [bilibili-cli](https://github.com/public-clis/bilibili-cli)，需要 Python 3.10+ 和 [uv](https://docs.astral.sh/uv/getting-started/installation/)。以下安装固定为已核对凭据格式的版本：

```sh
uv tool install bilibili-cli==0.6.2
bili login
chmod 600 "$HOME/.bilibili-cli/credential.json"
test ! -e bilibili-auth/credential.json && install -m 600 "$HOME/.bilibili-cli/credential.json" bilibili-auth/credential.json
```

用目标创作者账号扫码。独立复制凭据后，桥接服务的群内刷新不会改写外部 CLI 的文件；不要打印 Cookie，也不要反复登录绕过风控。

在 `.env.local` 设置：

```dotenv
SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED=true
SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK=I_UNDERSTAND_BILIBILI_WEB_API_ACCOUNT_RISK
SHADOWBRAIN_BILIBILI_CREDENTIAL_FILE=./bilibili-auth/credential.json
SHADOWBRAIN_BILIBILI_ACCOUNT_ID=你的创作者UID
```

启动时会检查 `SESSDATA`、`bili_jct`、`DedeUserID`（兼容 CLI 的小写字段）及账号匹配。凭据不完整或账号不符时停止，重新核对登录账号；不能用手填假值跳过检查。

### YouTube

在 Google Cloud 启用 YouTube Data API v3，配置 OAuth 同意屏幕并创建 **Desktop app** 客户端。将下载的客户端 JSON 放到 `secrets/youtube-client.json`，然后：

```sh
chmod 600 secrets/youtube-client.json
./shadowbrain-comment-bridge auth youtube \
  --client-file ./secrets/youtube-client.json \
  --token-file ./secrets/youtube-token.json
```

在同一台电脑浏览器打开命令输出的授权链接，选择拥有目标频道的账号。该流程使用本机回调，先在桌面电脑完成授权再传输给服务器；不要在无浏览器服务器上直接使用远端 localhost 回调。详情见 [YouTube 授权与配额](comment-bridge.md#youtube官方-data-api-与-oauth)。

设置 `SHADOWBRAIN_YOUTUBE_ENABLED=true`、`SHADOWBRAIN_YOUTUBE_CHANNEL_ID`，保留示例中的客户端和 token 文件路径。可用 `discover-youtube --client-file ... --token-file ...` 查询授权频道。若仍处于 Google OAuth 测试模式，应按 Google 的令牌有效期和测试用户限制管理授权。

只使用一个平台时，另一个保持 `ENABLED=false`，不需要创建它的凭据文件。两个平台可共用飞书群，也可设置 `SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID` 使用第二个群。

## 4. 启动与验收

直接运行：

```sh
./shadowbrain-comment-bridge run
```

保持此进程运行，在另一个终端进入同一目录检查：

```sh
./shadowbrain-comment-bridge status
```

首次轮询仅建立基线，不推送历史评论；首次读取完成后才连接飞书，耗时取决于评论数量和平台节流。`status` 仅证明能读取队列，不代表平台联网成功。按以下顺序自行验收：

1. 在配置的群 `@机器人 help`，确认收到帮助。
2. 用受控测试账号发表一条新评论，确认飞书通知正确。
3. 人工直接引用该通知并 `@机器人` 回复，核对平台只收到一次。
4. 删除/拉黑先验证二次确认与取消；只有确实允许删除的测试内容才确认执行。

长期部署使用 [Docker 指南](../deploy/comment-bridge/README.md)，它负责后台运行、日志轮转及单独的数据卷。应用不会安装后台服务或修改现有服务器。

## 常见首次配置问题

| 现象 | 检查 |
| --- | --- |
| 提示至少启用一个平台 | 示例默认全部关闭，选择至少一个平台 |
| 凭据无效或权限错误 | 文件须为普通 `0600` 文件，目录 `0700`，运行用户可读；Docker Linux 还要匹配 UID |
| 没有旧评论通知 | 正常，首次只建基线 |
| 引用回复无效 | 直接引用本机器人通知，核对群、人员授权、消息事件权限，并 @机器人 |
| B站 state=stopped | 停止操作并人工核对风险原因；不要重启、换凭据或重置数据库绕过 |
| failed / unknown | 先核对平台实际结果，不自动重发或恢复旧数据库 |

日志和提问中只提供错误类别、版本及脱敏配置，勿附 Cookie、token、二维码或完整数据库。
