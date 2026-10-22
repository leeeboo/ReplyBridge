# Comment Bridge · 视频评论助手

独立的 B 站 / YouTube 评论监控与飞书机器人项目，从 ShadowBrain 拆分，当前功能版本为 `0.1.19`。可选启用 B 站弹幕巡检：每 10 分钟最多 100 条，由本机 Codex 分析后向飞书发送异常提醒。

- 新评论推送到飞书；群成员引用机器人消息即可发布平台回复。
- B 站评论点赞、Cookie 失效提醒和群内二维码重新登录。
- YouTube 视频标题查询，点赞不支持时提供手动操作提示。
- 日本时间 21:00–次日 03:30 降至每 20 分钟轮询。
- 错误合并告警、持久化去重与限频；B 站风控持久停用并立即尝试飞书告警，普通读取故障冷却退避。

## 本地验证与构建

在本目录执行，需要 Go 1.26.5：

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

首次 B 站 Cookie 可由独立授权工具取得；已有部署继续使用原外挂 Cookie。机器人运行期间支持在群内扫码刷新。`dist/` 若存在，仅为迁入的历史发布包，当前版本应从源码重新构建。
