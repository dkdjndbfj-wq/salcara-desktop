# 远程上下文与原生压缩收尾 · 2026-10-15

本轮补齐手机远程会话的真实上下文观察、手动压缩和执行状态。Claude 仅覆盖 API 接入；SIWC 暂缓。此文不代表全部 Codex/Cockpit 产品能力已对齐，也不替代真机和实际上游验收。

## 已实现

- `session.context` 只观察当前 Bridge 拥有的运行时，不启动或恢复 worker、不发模型请求，也不用累计计费用量填充上下文。
- Codex 使用 `thread/tokenUsage/updated.tokenUsage.last.totalTokens` 和 `modelContextWindow`；界面标为“最近上下文”。缺失时保留未知，压缩后等下一次可信观察。
- Claude 使用原生 `get_context_usage` 的 summary 估计，窗口保留 effective 语义。旧 CLI 可能忽略 `detail` 并执行完整计数，因此仅已确认支持 summary 的版本启用；本机 2.1.220 不触发该读取。`result.usage/modelUsage` 不作为当前占用。
- `session.compact` 绑定精确 session、CLI 控制面、API 账号、来源授权代次、运行时 revision 和 operationId。检查覆盖预检、原生写入边界及响应返回，A→B→A 不会使旧操作重新获得授权。
- Codex 调用 `thread/compact/start`；空 RPC 回执只代表接受，完成要有匹配的原生压缩事件。Claude 仅在目录明确标记内置 compact 时使用 `/compact` 原生 user frame，完成要有 manual compact boundary 和 native result。不产生伪造的手机用户气泡。
- 结果状态为 requested/running/completed/failed/unconfirmed；独立 `blocking` 字段说明该操作是否仍占用当前运行时。完成回执尚未退出时继续阻止发送，已退出但结果未确认时解除占用。旧 worker 回执不锁住新 worker。
- 同一次操作幂等；过期、重复、迟到、错会话或旧连接通知不能重放执行或污染新状态。超时不等于成功，也不会杀死共享连接或自动重新压缩。
- 手机在输入框上方以一行 11px 文本显示占用、状态和压缩按钮；未知上限不显示伪造百分比。增加 `/compact` 命令入口，复用按钮的同一套校验并保留其余草稿，不发送普通聊天消息。
- 压缩占用不会隐藏已确认原生轮次的“停止任务”入口；仍使用原有精确 turn 校验，没有把未知或外部轮次当作可停止任务。
- `context.updated` 在手机只作为失效信号，重新进行受来源保护的读取，不直接采纳事件中的上下文快照。压缩占用期间只轮询状态，不重发压缩。

## 来源与生命周期

查询和压缩都要求显式 Hub/adapter 能力。原生桌面或只读历史不会回落到 CLI 执行；运行中、待审批、子任务仍活动、来源已变和权限未确认时拒绝压缩。

手机的读取句柄包含连接、配对、账号和已观察的来源变更代次；换来源、换会话、卸载和迟到回包均隔离。发送、继续、模型及 API 切换与当前压缩占用联动。`blocking` 必须由服务端明确提供，不能仅根据结果状态猜测。

独立复核发现：受限站点的只读请求需要 requestId 绑定来源授权，通用回执路径原先会因此为每次状态读取建立 24 小时持久回执。频繁读取会挤占正常发送容量；关联的来源挑战也需要精确清理。现精确将 `session.context`、`models.list`、`features.list` 放入随 HTTP 取消的只读路径，保留 UUID、来源验证及返回前复核，且不允许通过旧缓存回执绕过来源校验。只有已验证且指针身份仍匹配的 challenge 才会清理，防止旧读取删除新授权。`session.compact` 继续使用持久幂等回执，workspace 文件读取原有规则保持。

Claude 最后一轮还补了未确认结束至 worker 退休之间的占用间隙：即使 pending 已移除，只要同一回执仍占用活动 worker，就保持 `blocking=true`。确定退休后才解除；旧回执和普通任务 busy 不影响它。

## 验证

全部使用合成运行时/本地 fixture，没有真实 OAuth、付费推理、远程部署或生产数据操作。

| 检查 | 结果 |
| --- | --- |
| Bridge `go test ./... -count=1 -timeout=300s` | 通过；agents 252.329s，console 88.445s，hubclient 60.522s |
| Codex context/compact 最新 ordinary / race | 通过，0.496s / 1.596s |
| Claude context/compact ordinary / 广覆盖 race | 通过，0.646s / 23.441s；末轮退休间隙补丁定向 ordinary 2.183s / race 3.450s |
| Bridge context 调度 race（含来源 A→B→A） | 通过，4.963s |
| 手机 context parser、组件、transport、slash | 4 suites，53 tests 通过 |
| 手机 ThreadView compact/slash/目录变更整合 | 24 tests 通过；其余 273 项该次未运行 |
| 手机 compact/草稿来源/精确停止最新整合 | 3 tests 通过；其余 295 项该次未运行，与上一行有重叠 |
| Hub 三类只读回执、nonce、撤销与取消 | 每类 258 次读取；普通定向 5.977s，workspace/admission 0.668s，race 26.671s，包 vet 通过 |
| 手机 TypeScript | 最新 context/slash 修改后通过 |
| 浏览器实际 ContextBar 组件 | 320px 浅深色、接受/完成/未确认；无横向溢出、只执行一次压缩、控制台 0 error/warning |
| Bridge 静态检查 | context 修订前全仓通过；最后一次被另一会话进行中的技能外观符号影响，待该批整合后统一终验 |

Bridge 全仓测试通过之后，另一会话继续修改结构化引用与技能外观，因此这里是明确时间点的验证证据，不能代表那些后续改动也已通过。

## 证据与未完成边界

Codex 接口依据本机 CLI 0.145 生成的协议 schema，包括 ThreadTokenUsage、ThreadCompactStart 和原生通知。官方文档入口为 <https://developers.openai.com/codex/app-server>；本次网络读取遇到 Vercel 检查，空的本地缓存不能作为已读取文档证据。Claude 依据已取得的官方 SDK 0.3.300 类型/控制契约及本机 CLI 2.1.220 行为差异，不把 SDK 最新能力直接当作旧运行时能力。

后续阶段已接入原生会话管理、Codex 审查/Goal、独立侧边提问，并与协作会话整合结构化引用、技能外观和文字选择。实现合同与新增验证见 [远程会话工作流收尾](REMOTE-SESSION-WORKFLOWS-20261015.md)。上表仍保留本 context 阶段的时间点，不代表后续修改的整树结果。手机本地别名与原生改名、审查展示与原生审查启动、普通继续与 Goal 均保持独立。

真机、弱网休眠、实际账号/原生运行时和发行包验收仍需分别记录。后续 Claude 合法 remote B/local A 来源修复也应以新文档的生命周期测试为准，不能仅引用本阶段旧的全仓测试。
