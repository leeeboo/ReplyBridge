# ReplyBridge（回声桥）

独立的 B 站 / YouTube 评论监控与飞书机器人项目，从 ShadowBrain 拆分，当前功能版本为 `0.1.22`。可选启用 B 站弹幕巡检：每 10 分钟最多 100 条，由本机 Codex 分析后向飞书发送异常提醒。

- 新评论推送到飞书；群成员引用机器人消息即可发布平台回复。
- B 站评论点赞、Cookie 失效提醒和群内二维码重新登录。
- YouTube 视频标题查询，点赞不支持时提供手动操作提示。
- 两个飞书群均支持 `@机器人 帮助/help`；“踩”发起删除、“-1”发起拉黑，由发起人本人二次确认后执行。
- 日本时间 21:00–次日 03:30 降至每 20 分钟轮询。
- 错误合并告警、持久化去重与限频；B 站风控持久停用并立即尝试飞书告警，普通读取故障冷却退避。

[![CI](https://github.com/leeeboo/ReplyBridge/actions/workflows/ci.yml/badge.svg)](https://github.com/leeeboo/ReplyBridge/actions/workflows/ci.yml)

采用 [MIT 许可证](LICENSE)，第三方组件保留各自许可，见 [第三方声明](THIRD_PARTY_NOTICES.md)。支持 macOS 和 Linux；Windows 请使用 Linux 容器或 WSL2，不支持直接运行 Windows 二进制。

首次使用从[安装指南](docs/quick-start.md)开始，选择 B站、YouTube 或同时启用。可从 [Releases](https://github.com/leeeboo/ReplyBridge/releases) 下载二进制和 SHA-256 校验清单，也可从源码构建。

## 本地验证与构建

在本目录执行，需要 Go 1.26.10：

```sh
go test ./...
go build -o shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge
./shadowbrain-comment-bridge version
```

项目有独立 `go.mod` / `go.sum`，不依赖父目录、`go.work` 或本地 `replace`。B 站基础客户端仅保留登录、HTTP、签名、节流与评论能力；知识库、字幕同步、MCP 和 ShadowBrain OAuth 服务不在本项目内。

## 配置与部署

复制 `.env.example` 为 `.env.local`，填写配置；密钥、平台授权和数据库使用独立外挂文件，不进入源码或镜像。

- [完整使用、授权与权限说明](docs/comment-bridge.md)
- [Docker 构建、外挂文件、迁移与回滚](deploy/comment-bridge/README.md)

为兼容现有服务器，二进制名 `shadowbrain-comment-bridge`、`SHADOWBRAIN_*` 配置名、数据库格式、Compose 名称与 volume 名保持不变。拆分源码无需替换授权或迁移线上数据库。

首次 B站登录使用公开的 `bilibili-cli`，安装与凭据导入步骤见[安装指南](docs/quick-start.md#b站)。已有部署继续使用原外挂 Cookie。机器人运行期间支持在群内扫码刷新。`dist/` 若存在，仅为迁入的历史发布包，当前版本应从源码重新构建。

版本变更见 [CHANGELOG](CHANGELOG.md)。CI 只执行隔离测试、构建和漏洞检查，不包含服务器部署或真实平台操作。

## 开发与打包

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
# 先按部署文档构建测试镜像；仅使用临时数据和网络禁用容器。
sh scripts/test-compose.sh shadowbrain-comment-bridge:0.1.22
# 生成四个平台的安装包及校验清单；拒绝覆盖同版本目录。
sh scripts/package.sh
```

发布前核对源码版本、第三方声明、全部测试与 CI，再创建对应 Git 标签和 GitHub Release。打包脚本只收集公开文件，不包含运行配置、凭据或数据库；构建/发布不操作服务器。
