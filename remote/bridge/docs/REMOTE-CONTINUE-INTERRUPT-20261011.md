# 远程会话停止与继续契约（2026-10-11）

## 目标

手机端是电脑 Agent 会话的远程控制面板，不是第二个独立推理端。本次实现只作用于 Bridge 拥有并能够验证的 CLI 会话；原生 Codex/Claude Desktop 的只读会话不会被 CLI 偷换接管。

## Continue

- `session.continue` 使用同一个 `sessionKey`，要求 `expectedTurnId` 是手机从新鲜会话快照观察到的上一轮终态 ID。
- Bridge 在 Agent 适配器内执行终态 CAS；Codex 通过同一 thread 的 `thread/resume`（需要时）后调用 `turn/start`，因此历史、工作目录和会话身份保持不变。
- `operationId` 同时作为命令幂等键；网络超时重试不会创建第二个继续操作。
- 继续不是 Goal resume，也不是普通 `session.send` 的文案替换；不接受图片，模型/推理覆盖仍按当前 API 和 Agent 能力校验。
- 终态不明确、会话正在运行、子任务仍在运行、身份/API 已切换时拒绝发送。

## Stop

- `session.interrupt` 必须带准确的 `expectedTurnId` 和 UUID `operationId`。旧的无 ID `Agent.Interrupt` 入口保持拒绝，避免迟到的停止命令误停下一轮。
- Codex 只向原生 `turn/interrupt` 发送精确 thread/turn；Claude Code 仅在 Bridge 自己拥有且能确认的单一 worker、无子任务时停止。排队中的多轮 Claude worker 不会被“一刀切”。
- 返回值区分 `requested`、`stopped`、`already_finished`、`unconfirmed`；手机只有收到对应终态事件才清除“停止待确认”。
- 停止会暂停手机待确认消息和队列的自动重发，但不会声称已经撤回一个已经跨过网络边界的请求；原收据保留供用户核对。

## 外部 Claude 会话

旧的 transcript 年龄、最近修改时间或自然语言 `result` 不能证明外部 Claude 已空闲。没有 Bridge 本进程的 Wait-confirmed 完成收据时状态为 `unknown` 且不可控；收据对应的文件身份、大小、mtime 发生变化会立即撤销控制权。

## Claude Desktop 的 API 接入边界

Claude Desktop 的 API 接入和原生聊天会话控制是两条链路，不能混称为“反代”：

- `ClaudeDesktop3PService` 只负责在用户明确选择后，给 Claude Desktop 写入受校验的第三方/API 配置；切换前保留本机备份，支持模型目录发现与恢复。远程地址不能伪装成本机专用协议转换入口。
- 手机选择 `claude-desktop` 的 API 时，Hub/Bridge 使用统一的 Claude API 账户目录和来源校验；这会同步 API 卡片和 Claude Desktop 的第三方配置，但不会接管标准账号聊天，也不会把旧 Desktop transcript 变成可写会话。
- 真正的手机“继续/停止/发送”仍要求 Bridge 拥有并确认 Claude Code worker（同一 `claude:<id>` 会话）；仅安装 Claude Desktop 或仅有它的历史文件时，能力保持只读。这样既支持 Claude Desktop 接 API，也避免把没有官方控制端点的原生桌面进程伪装成可远程控制。
- 若用户选择的 API 不是 Anthropic Messages 协议，只允许经过本机专用 loopback 兼容网关的明确路径；远程公网 URL 不会被当作协议转换器。

## 当前验证

- 手机 `npm run typecheck` 通过；当前全量 Jest 为 96 套件、1522/1530 项通过，8 套件/8 项既有跳过；新增附件 scope/abort、await 栏栅与 queue handoff 回归已包含。
- 手机控制/收据/聚合定向测试通过（包括终态 ID 冲突、继续收据重试不降级、队列停止竞态）。
- Bridge 整仓普通测试通过（各测试包 exit 0）；其中 `internal/agents` 162.956 秒。`internal/agents`、`internal/hubclient`、`internal/protocol` 的 race 复验通过（分别 208.666/31.039/1.280 秒）；整仓 `go vet ./...` 与 diff 检查通过。
- 尚未用真实 Codex/Claude 账号或付费模型做运行验证；需要在用户自己的电脑上验证实际 Agent 版本是否广告这些能力。Claude Desktop 的第三方/API 配置和恢复使用 synthetic profile 测试；原生桌面只读会话仍明确受官方接口限制。

