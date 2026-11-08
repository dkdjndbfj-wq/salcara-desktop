# 远程会话权限与模型切换修复 · 2026-10-12

## 范围与实际问题

本轮对应手机对话权限入口缺失、模型选择无效的反馈，与账号卡片/导入弹窗视觉审阅并行。未发布、未部署、未访问实际账号或生产服务器，测试全部采用合成数据和本机模拟传输。

已定位：

1. 旧会话没有权限模式入口，协议也只有新建任务的 legacy `approval`，不是 Codex 的完整权限语义。
2. 原生桌面模型选择被整条链路硬关闭。不能只取消 UI 的 disabled：需要宿主实际工具 schema、授权 lease、来源身份、参数与重试回执一致。
3. 模型/强度手势没有来源代次围栏；等待消息回执时仍能点击模型。
4. Bridge 的 `requireAgentCapability` 漏了 `continue` 和 `preciseInterrupt` 分支，导致已声明的继续/精确停止能力仍被拒绝。

## Codex 权限合同

新增 `permissionMode`，独立于旧 Claude/旧版 Bridge 的 `approval`：

| 模式 | approvalPolicy | approvalsReviewer | sandbox |
| --- | --- | --- | --- |
| 请求批准 `ask` | on-request | user | workspace-write |
| 帮我批准 `auto_review` | on-request | auto_review | workspace-write |
| 完全访问权限 `full_access` | never | user | danger-full-access |

依据：[官方沙箱说明](https://learn.chatgpt.com/docs/sandboxing)、[自动审核说明](https://learn.chatgpt.com/docs/sandboxing/auto-review)、[App Server 协议](https://learn.chatgpt.com/docs/app-server)，并核对本地隔离生成的 `work/conversation-protocol-schema/v2/ThreadStartParams.ts`、`ThreadResumeParams.ts`、`ThreadStartResponse.ts` 与 `ApprovalsReviewer.ts`。

`auto_review` 不是直接批准一切。它保留工作区沙箱，由 Codex 自己决定审核结果；Salcara 不用 legacy `auto_all` 冒充。宿主管理策略、账号支持仍具有约束力。

### 执行边界

- 只适用于声明 `permissionModes` 的 Codex CLI/app-server adapter；不是任意 Agent 通用标签，更不会由原生桌面请求暗中转到 CLI。
- 新建任务先核对 `thread/start` 返回的实际 policy/reviewer/sandbox；已有会话以无输入的 `thread/resume` 设置并核对同一会话，再发送 `turn/start`。不修改全局 config.toml。
- 同一已加载会话已确认相同模式时不重复 resume。进程重启/卸载再恢复时必须重新确认，不能把旧 epoch 的标签当确认结果。
- 正在执行、等待审批、有活动子任务或状态未知时拒绝权限切换；不会中断在跑任务。
- 旧宿主忽略新字段、返回其他权限或确认结果不完整时不发消息，不自动降级，也不显示成功。已有会话进入权限待确认状态，须明确重新选择并确认后才能恢复发送。
- 完全访问要求电脑 `AllowPhoneAutoAll` 明确开启。执行前与异步权限确认后重新检查；关闭此选项不停止已被接受的任务，但不再接收新的完全访问任务。
- 旧会话未主动选模式时不覆盖；手机选择被冻存在消息、继续操作、新任务和离线队列的 payload 内，重试不能借用当前 UI 选择。
- `SessionInfo.permissionMode` 只表示运行时已确认的模式。缺省/清空时显示跟随电脑，而不是凭本地选择假装已应用。
- Codex 已弃用的 `untrusted` 请求策略改为 `on-request`；Claude 原有 `auto_edits` 语义不改。

### 文件

- Bridge：`internal/agents/codex_permissions.go`、`codex.go`、`internal/hubclient/permission_modes.go`、`dispatch.go`、`aggregation.go`、`internal/protocol/protocol.go`。
- 手机：权限类型、能力投影、ThreadView 入口、store 命令、new-task/delivery/drafts 快照与重试，以及对应测试。

## 尚不能混称的能力

Codex 原生桌面工具的批准请求响应与修改整个会话权限是两回事。当前原生工具没有已验证的权限模式修改参数，因此原生桌面会话继续跟随电脑权限；不能把 CLI 权限覆盖宣称为原生桌面全功能复刻。

模型目录是选择提示，不等于模型可用权限或账号套餐证明。真实宿主拒绝、模型容量不足、来源授权不通过都必须保留真实错误。受限 Hub 的原生实际 provider 来源证明边界不因本次模型 UI 修改而放开。

## 验证记录

权限模式映射、旧宿主缺字段、错误 reviewer/sandbox、错误会话、活动状态、异步状态变化、未确认阻断与明确恢复、桌面晚撤销、start/send/continue 透传、其他 Agent 拒绝和能力开闭均有模拟回归。初次广选 provider 测试组合的 90 秒总时限不足；原 provider guard 测试单独使用 300 秒上限重跑，74.109 秒通过，没有改生产行为来缩短测试。

### 收尾审查追加

- 首次 `thread/read`、恢复、权限切换与发送共用单线程 admission lock，避免两个手机请求交错改变对方的权限。
- 未显式配置过的原会话不会继承新任务默认值；恢复不主动写入 approval/sandbox。明确降权时也不会短暂恢复旧 `auto_all` 或启用本机自动批准。
- 恢复返回的线程 ID、状态、运行时连接、线程实例与 turnVersion 全部复核；迟到响应不能覆盖新任务状态或向另一会话发送。
- 原会话返回危险沙箱但 reviewer 元数据不足时，不伪造完整权限标签；仍执行电脑完全访问许可的动态检查。撤销许可不会终止已有任务，但阻止新的危险权限输入。
- 模型确认只更新所属活动/已结束轮次，旧轮次的模型返回不能覆盖新轮次选择。
- 原生桌面模型支持按宿主实际 schema 协商；手机模型列表与发送均冻结真实 Applied API，CLI RemoteAPI 不可串入。来源变更后清空旧原生选择，不继承 CLI 默认模型。完整实现见 `NATIVE-DESKTOP-MODEL-OVERRIDE-20261012.md`。

### 已取得结果

- 手机全量：100 套、1604 条通过；既有 8 套 / 8 条跳过。最后的原生来源重置及标题更新另以 5 条窄回归通过；typecheck 通过。
- 手机主要权限/模型组合：7 套、254 条通过；模型选择器整套 32 条通过。并行构建时一次 5 秒默认测试超时，单独以 20 秒测试阈值重跑通过，没有放宽业务网络超时。
- Bridge 最终权限/首次恢复/模型确认/许可撤销定向 race：agents 与 hubclient 两包通过。
- Companion 全量 101 条通过；最终 schema 加严后的受影响 37 条通过。原生模型相关 Go 三包定向、race、vet 通过。
- 第一轮 Bridge 全量使用修改前快照，在默认 effort 测试失败；该路径已修正并单独通过。随后 agents 的旧测试仍假定全局自动批准会覆盖原会话，已调整 fixture/显式批准交互，不能为通过测试恢复不安全行为。最终完整包结果另附于下方。

这些是合成传输和组件回归，不替代真实手机、真实原生桌面和账号授权的端到端验收。没有发布安装包或部署。Companion 0.4.2 当前工作区尚未发布；若已有同版本测试安装，正式分发需递增 payload 版本，不能依赖同版本覆盖安装。

最终合并复验：`go test ./internal/agents ./internal/hubclient -count=1 -timeout=600s` 两包通过（273.828s / 53.771s）；最后显式降权恢复窗口另经定向 race 通过（1.991s / 3.422s）。最终两包 vet 及 Bridge diff check 通过。第一轮全仓其余包均通过；新增账号清理的独立验证记录在 `UNIFIED-ACCOUNT-ADD-CLEANUP-20261012.md`。手机最终 typecheck / diff check 通过。
