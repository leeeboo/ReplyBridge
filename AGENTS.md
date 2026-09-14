# ReplyBridge（回声桥）

独立的 B站 / YouTube 视频评论与飞书双向桥接项目。后续修改在本项目完成，不回写 ShadowBrain；文档和对外介绍使用 ReplyBridge（回声桥）。

## 兼容性与范围

- 品牌更名不自动重命名目录、Go 模块、`shadowbrain-comment-bridge` 二进制、`SHADOWBRAIN_*` 配置、数据库、Compose 或 volume；仅在明确要求时修改这些兼容性标识。
- Go 版本和依赖以 `go.mod` / `go.sum` 为准，应用版本以命令入口源码为准。`dist/` 是历史发布包，不能据此判断当前版本。
- 保持独立构建，不引入父项目、本机绝对路径或本地 `replace` 依赖。
- 精准修改，保留用户及其他任务的改动；提交时只纳入本次范围。没有 Git 仓库时，不因检查或编辑自动初始化 Git。

## 核心行为

- 首次轮询只建立基线，不补发历史评论。保留通知去重及评论与飞书消息的持久映射。
- 平台回复必须满足目标群、人员授权和直接引用本机器人通知的映射条件；不能用同话题或用户提供的评论 ID 绕过校验。
- 结果不确定的外部动作保持 `unknown`，不自动重试；恢复读取不代表可以重放失败的回复、点赞或通知。
- B站读取冷却期间不得通过重启、并行请求或写操作绕过限制。平台能力差异以适配器和使用文档为准。

## 数据与操作边界

- 同一 SQLite 数据库仅运行一个实例，保留 runlock。迁移和回滚须维护数据库与配套凭据的一致性。
- 凭据使用受限权限的外挂文件；不将密钥、Cookie、token 或评论正文写入日志、公开文档和发布包。
- 诊断默认只读；已授权修复、验证和发布的步骤连续完成，仅暂停缺少授权或必要条件的环节。
- 构建、公开源码、发布制品、升级服务器和真实发消息分别按任务范围判断授权；发现现有凭据不代表获准使用。

## 开发与验证

命令入口位于 `cmd/shadowbrain-comment-bridge/`，持久化与任务处理位于 `internal/commentbridge/`，平台适配位于 `internal/bili/`、`internal/youtube/` 和 `internal/feishu/`。

- 复用现有 Go 测试，按改动选择包或用例，例如 `go test ./internal/commentbridge -run TestBridgeEndToEndAndReplayDeduplication`。
- 授权、引用映射、去重、重试、存储或调度逻辑变更须覆盖对应回归；并发变更按需加 `-race`。纯文档修改核对内容与引用即可。
- 跨包修改或发布准备使用 `go test ./...`、`go vet ./...`，需要构建时使用 `go build -o shadowbrain-comment-bridge ./cmd/shadowbrain-comment-bridge`。不为小改动固定运行全部检查。
- 报告实际执行结果；源码测试、制品构建、容器健康和平台真实链路分别核验，未执行项不能写成通过。

## 按需资料与技能

- 配置、权限、平台行为和人工操作：[使用文档](docs/comment-bridge.md)。
- Docker 构建、外挂数据、切换与回滚：[部署文档](deploy/comment-bridge/README.md)。
- 漏评论、通知或引用回复异常、登录失效、队列积压：[replybridge-diagnose](.agents/skills/replybridge-diagnose/SKILL.md)。
- 开源发布准备、制品构建或已授权部署：[replybridge-release](.agents/skills/replybridge-release/SKILL.md)。

只读取当前任务需要的章节。项目技能随仓库维护，不依赖个人全局技能、固定模型或私有服务器配置；普通编辑不必加载上述技能。
