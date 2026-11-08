# 最新消息编辑的原生历史契约

`session.revert` 仅接收 `sessionKey`、`expectedTurnId`、`expectedMessageId`、UUID `operationId`、`controlSurface: "cli"` 及可选 `accountId`。编辑后的内容通过原有发送链单独提交，撤回操作不会自动启动模型。

Agent 和会话都必须明确报告 `latestTurnRevert: true`。Codex 初始化后只向格式无效、不能对应用户会话的 thread ID 探测方法；未知方法、超时及无法区分的参数错误均不授予能力。2026-11-08 本机 0.145.0 的隔离探针确认 `thread/revert` 不存在，`thread/rollback` 存在。旧持久历史只使用已确认的 rollback，新 paginated 历史只使用已确认的 revert。

执行要求 Bridge 拥有且本进程已加载的持久会话、明确空闲的最新完整轮次、该轮恰好一条普通用户消息且原生 ID 精确匹配。运行中、子任务未完成、审批未结束、自动 Goal、压缩、外部或只读会话均拒绝。锁覆盖原生核验和写入；不通过删除本地显示模拟成功。

返回 `{status: "completed" | "unconfirmed", sessionKey, turnId, messageId, operationId, session?}`。只有原生响应及保留历史边界确认后才返回 completed 并广播 `session.reverted`（`id` 为用户消息 ID）。消息及其回复从原生历史移除，但磁盘文件修改保持原样。

每次写入前先用 `atomicfile` 保存操作意图。未确认的操作保持阻塞，任何重试都不会再次调用原生写入。重新打开历史或重查同 UUID 会只读核对：目标已消失且最新边界等于保存的上一轮，才确认完成并释放阻塞。内部 RPC 错误可能发生在提交之后，按未确认处理。

已确认的撤回记录随会话读取补发，手机离线后重新打开也能得到墓碑。每个会话最多保存 256 条操作记录，不静默驱逐幂等记录。此实现不声称支持其他 Agent，也不声称能够控制另一个原生桌面进程的会话。

官方新协议：[OpenAI app-server](https://developers.openai.com/codex/app-server#revert-thread-history)。新协议不支持 ephemeral 或 legacy 历史，响应的 `thread.turns` 为空，不能将它当作整个历史被删除。
