# Salcara 聚合内核审计（2026-10-09）

本轮只改内核、协议和测试；桌面端/手机端的 UI、动效、布局和文案没有改动。

2026-10-10 状态修订：本机账号池与远程来源授权分开。远程固定所选账号，不继承池成员；Bridge 已实现二次来源验证和配置快照，但可信 issuer 签发/手机获取 grant、真实 OAuth 生命周期和原生进程 provider 证明仍未闭环。本文“已完成”不代表完整的账号反代与商业化验收。

2026-10-11 后续复核：最新状态见 [Cockpit 对齐矩阵](COCKPIT-PARITY-REVALIDATION-20261011.md) 和 [反代与聚合继续修复](PROXY-AGGREGATION-CONTINUATION-20261011.md)。官方账号现已有显式导入、legacy CLI 登录、刷新所有权、保护存储和 Responses 执行器，不能再描述为“只有数据模型”；真实上游登录、额度/套餐、受限官方来源连接器和原生 provider 证明仍未验收。下方旧测试数量仅属于 10 月 9 日的历史基线。

## 已完成的聚合边界

- API 库与 Agent 卡片分离：一条账号记录可被 Codex、Claude Code、Claude Desktop 或扩展 Agent 引用，不复制 Key。
- `api_key`、`reverse_proxy`、`oauth`、`official_account` 是来源分类；来源类型不会被误当成 Agent 类型。
- Codex、Claude 和额外注册的适配器经过统一 `AgentDescriptor`；未知适配器不能被猜成 Codex/Claude，必须声明自己的 API family 和能力。这是扩展框架支持，不表示已经交付 Hermes 等工具的完整执行器。
- 扩展 Agent 不再只“能启动”：会话目录、历史打开、共享 API 选择和状态卡片都按它自己的 `id/apiFamily` 聚合；没有显式 API 选择时不会偷继承 Codex/Claude 的活动账号。
- GPT、Claude、Grok、DeepSeek 等混合模型按模型目录、精确模型规则和协议转换路由；未知模型失败关闭。
- 账号池显式使用 `poolEnabled + poolGroup`。当前 Agent 未启用池时，其他池成员不会影响它；启用池后只在明确分组内选择。
- 高优先级、权重、并发、冷却和会话粘性均在电脑本地执行。粘性按 Agent/池/会话命名空间隔离，Key 指纹变化会清除旧健康状态。
- `/v1/models` 是只读联合目录，不占推理并发；真实发送仍在发送前重新选择并固定账号。切换 Key 或删除成员不会中途替换正在进行的流。
- Hub 的 `open/restricted/disabled` 是设备远程执行来源策略，与资源模式、账号池和模型调度分离。

## 暂不虚构为“已完成”的能力

- 官方账号 legacy Codex CLI 登录/导入、刷新、过期分类和执行器已有实现，真实 OAuth 流程仍需实测；其他供应商尚未完成；官方 usage/套餐连接器与剩余订阅 hint 已实现，真实账号验收仍未完成。
- 受限 Hub/Bridge 已有 Ed25519 grant、来源 challenge、签发器框架和执行端二次验证；受限 official/oauth/reverse_proxy 的可信连接器及原生实际来源证明仍缺，缺少证明时拒绝执行。
- 计费、余额、套餐、请求审计和图片任务槽位属于上游 Hub/Sub2API ProviderAdapter，不由本地 Bridge 猜测或伪造。

## 验证

- Desktop：`go test ./internal/...`、`go vet ./internal/...` 通过。
- Hub：`go test ./internal/...`、`go vet ./internal/...` 通过；覆盖签名 grant、篡改、重放、重复键、大小写混淆、尾随 JSON 和截断命令。
- Mobile：`npm run typecheck` 通过；81 个 Jest suite 中 73 通过、8 个既有 skip；991 个测试中 983 通过、8 个 skip。

这些测试不能替代真实验收：Hub 重启、电脑重启、A→B 双站切换、代理超时、Android 长时间后台和真实 Codex/Claude Desktop 会话仍需在目标发行环境执行。
