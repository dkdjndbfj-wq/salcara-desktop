# 通用 Agent 接口与审批链路修复（2026-10-13）

范围：继续落实用户明确的“多个原生接口，输出一个手机接口”。本轮是增量内核修复与必要的队列交互，不是全部 Agent 原生桌面功能已经齐全的声明。保留现有视觉风格、问答提交后粒子消融/输入框恢复；未提交、发布或部署。

## 1. 架构与链路

手机不直接实现 Codex / Claude 私有执行协议。电脑端分别连接原生运行时，各适配器将其命令与事件转换成既有 `Agent / AgentDescriptor / SessionInfo / Event` 契约：

```text
Codex 原生接口 ───── Codex adapter ───────┐
Claude Code 原生接口 ─ Claude adapter ─────┤
Desktop 支持的接口 ── Desktop adapter ────┼─ 公共协议 ⇄ Hub ⇄ 手机通用界面
未来 Agent 接口 ───── 对应 adapter ───────┘
```

公共协议统一表达，不统一或模拟厂商执行语义。供应商 API/模型适配与 Agent 会话控制适配独立。尤其不能因为 Claude 和 Codex 共用一个 API，就让请求进入错误执行器；Desktop 历史的 client 标签也不赋予 CLI 或原生 Desktop 控制权。

每个变更操作仍经过：配对与站点身份 → 执行来源 → 精确 session / Agent / controlSurface → 实际可调用能力 → worker 与请求有效期 → 原生响应。Hub 负责投递、去重与事件回放，不替 Agent 猜权限。批准响应被接收不等于工具成功，工具结果另行返回。

## 2. 本轮实施

### 通用请求契约

- `protocol.Event.approvalOptions` 为可选版本化字段：`version: 1`、`decisions`（`allow / allow_session / deny`）、可选 `denyEffect: cancel_turn`。
- 适配器提供真实允许的操作；注册表复制与规范化数据，回答时再次强制校验，不能只把不支持的按钮藏起来。
- 手机对缺省旧描述采用一次允许/拒绝；未知版本或畸形描述不提供授予操作。问题和 Codex 原生 hook 不提供会话级授权。
- 原生 Codex `availableDecisions` 未传或为 `null` 使用兼容默认；显式空列表和只有不支持的结构化授权不伪造选项，直接返回明确协议错误。
- 仅可 `cancel` 而不可 `decline` 的请求在手机明确显示“取消本轮”，返回原生 cancel；不伪装成仅拒绝某个工具。
- Codex 网络审批保留目标/协议、命令、额外权限和环境信息；不把联网+命令误写成只有访问网站。注册表保留原生提供的 turn / item 关联，缺少关联时不猜测。
- Hub 事件体原样保留该字段；Bridge 事件预算计入描述开销。不需要 Hub 新增厂商分支。

### Claude 单请求授权及 MCP

- `ask` 显式使用 CLI `default`，`auto_edits` 交由 CLI `acceptEdits`。旧 helper 中的额外策略引擎不再按工具名自动批准。
- 旧 `permission-prompt-tool` 无法证明长期授权的精确范围；收到原生请求后只暴露一次允许/拒绝，不再把“本会话 Bash”扩大成同名工具的所有后续调用。
- MCP `notifications/cancelled` 的 request ID 对应独立 context，取消贯通 helper HTTP → Bridge 请求注册表；worker 还活着不代表旧请求仍有效。
- 进程生命周期与单请求生命周期分别校验；注册、快照、响应、验证后提交都受约束。取消最后一条请求会释放 waiting 状态，其他并发请求不被误清空；取消后不发出误导性的 allow resolved。
- 进程停止/替换的原子预留同时撤销 permission 与 elicitation 两条 lease；不等慢速进程/管道清理。helper 认证也检查 `stopped`，旧进程在该窗口不能新建授权请求或复用已有请求。
- 授权状态提交绑定具体 worker / elicitation lease，不能只用 session ID。注册表增删 revision 与最终状态提交检查阻止旧 pending 快照覆盖新状态；快照只复制不可变期限/channel，提交时不拿注册表锁，重试有界。无任务/子任务且无有效请求时回到 idle，不强制标为 running。
- Elicitation form hook 同样接入单请求取消，只保留实际支持的表单。OAuth/URL、不支持的 schema 仍不自动批准。
- helper 有硬行长 1 MiB、并发 32、响应 2 MiB、request ID 长度/总量限制；拒绝连接内重用 ID，防迟到取消命中新请求；EOF 撤销 HTTP 请求。
- 超限行在第一个超限字节即退出，不等待换行或又一个 64 KiB 分块。禁跟随 HTTP redirect，不转发本机授权 token。

### Claude 对话生命周期

- C04：结构化 origin、session、子任务标识区分主任务结果与通知结果；非 human 来源不消费主任务 FIFO。worker 内 UUID 有界去重；缺少 UUID 的旧 CLI 不凭内容猜重复。
- result 消费与当前 worker/live 更新在同一锁序下完成，旧进程与旁路结果不改变新任务终态。
- 解析原生 `terminal_reason / api_error_status / duration_ms`。旧 success 且非 is_error 的结果没有新字段仍可完成；明确 aborted 为中断，未知终态不算成功。
- C05：继续操作携带观察到的 terminal ID 到最终 writeTurn 比较，不能在等待期间另一个任务已开始后仍追加旧“继续”。
- 换模型/API 的空闲判断与 worker 退休合并成原子预留；停止操作同样绑定精确 turn。不会先检查空闲、再误杀中间刚收到的新任务。
- 非 result 帧也检查 worker 代际；子任务 busy 状态先更新再发布，避免慢事件接收者延迟执行保护。
- 主任务已结束但后台子任务仍运行时，保留真实 `LastTerminalTurnID`；后台任务结束只释放 busy，不伪造主任务结束，继续操作仍指向原主任务。
- 请求 context 在启动前、原权限检查成功后、等待 writer/Agent 锁后的最终入队点检查；尚未接收的取消消息不写入进程。已经接收的任务不因手机请求 context 随后取消而停止、回滚或重发；尝试写入后失败仍保留不确定交付语义。
- 所有 Claude live 状态提交统一在 Agent 锁内生成严格递增的 `UpdatedAt`，事件仍在锁外发送；同一毫秒的旧状态晚到也会被手机现有较旧时间戳检查拒绝。仅提交 live 变更时推进，磁盘历史加载仍保留原时间，不增加网络轮询。

### 手机动态队列

- 显示当前“待处理 N 项”，新增/撤销/过期实时改变 N，不把它当总步骤数。
- 当前项按请求 ID 锁定；前面插入的新项也不抢走正在编辑的表单。上下切换复用现有紧凑卡片。
- 问题草稿逐请求保存选项、原始输入（包括空白）、页码和展开状态；只驻留当前会话内存，不持久保存未提交回答。解析作用域同步隔离，重新配对即使复用 request ID 也不能拿到旧草稿。
- 已撤销/到期项不再可提交；旧点击回调在当前会话/能力/期限/原生 decisions 下重新验证。
- 问答队列控制放在同一无障碍 dialog 内，不能因 modal 无障碍焦点将切换按钮隔离在外。
- 分离“禁止提交回答”和“禁止全部操作”：仅支持 deny 的合法问题仍可拒绝，但离线、失效、处理中或待核对回执时不能借此发送另一项授权；若拒绝会取消本轮，明确标注其效果。
- 手机先检查 native Desktop 执行器，再检查通用 decisions；保留原有 `desktop_capability_unavailable` 错误合同，不改变原生不支持操作的处理路径，也不回退到 CLI。
- 保留问答已有持久回执、丢包重试与消融动画；普通工具批准仍使用既有命令回执，不在本轮宣称与问题 outbox 的交互全部统一。

### Claude API 专项核查与执行面

用户已明确 Claude Code 与 Claude Desktop 均只使用 API，不接账号登录。本轮不增加 Claude 登录或账号反代功能。

- `agents.status.execution` 单独说明可写执行器。Claude Desktop 卡片仍是 Desktop 展示身份，其 Code 执行器为 `{tool: "claude", controlSurface: "cli", entrypoint: "claude-desktop"}`。原生 Chat/Cowork 历史不因此获得写权限。
- 手机根据该描述冻结新任务目标；续聊验证真实 session 的 tool/执行面。不接受显式空、畸形或冲突的执行描述；旧版本仅使用既有内置兼容映射。不能根据卡片名称把原生 Desktop 请求转给 CLI。
- Claude `status: unknown` 或 `controllable: false` 不能接收新的发送/继续操作；发送前与最终投递前均检查。已有尝试的同 UUID 回执恢复仍保留，不制造新命令，不误删草稿。
- 底层 `StartWithOptions`、`SendWithOptions`、`spawn` 和进程配置生成统一验证 API 地址与 Key。删除空地址时继承本机 login/settings 的旧分支。检查发生在新建会话状态、复用或退休旧 worker 之前，不影响读取、停止及已产生的审批。
- 手机共享 `api-admission.ts` 检查 `configured`、`pending`、来源及不透明 API 句柄；Desktop Code 使用它自己卡片的已确认句柄。错误码统一为 `claude_api_required`。第一次校验到实际执行前 API 被移除时，不产生新任务；启动失败清除未成立的 Desktop 归属/导入标记。
- “跟随电脑”只接受已确认的电脑 API，不表示本机登录。未尝试的待发操作必须重新核对当前 API；已尝试的 Claude 操作只允许以原 UUID、原消息及原 API 句柄恢复回执，即使当前已换 Key 也不重构命令。该旧回执例外仅限 Claude，本轮没有拓宽 Codex/扩展的旧准入语义。
- Bridge 托管的 Claude API worker 使用独立 `--settings <私有文件>` 固定路由与认证模式，保留原有 Elicitation hook。仅设置子进程环境不足以抵御用户/项目 settings 的环境覆盖及热重载；本轮不改用户配置文件，也不取消组织策略。
- 当前选定的 API Key / Bearer 二选一；清空旧 OAuth、其他云提供商选择、额外认证头及组织路由变量。API Key 不内联到命令参数。目录在写入前完成权限设置，Windows DACL 仅当前用户与 SYSTEM，其他平台目录 0700 / 文件 0600。
- 私有配置只在实际 `cmd.Wait` 确认进程退出后清理。输出管道退休不等于进程退出；此时保留 API pin，防尚存活进程恢复旧配置。清理只删除准确文件与空目录，不递归删除。Unix 信号终止也视为已确认退出，避免永久禁止后续启动。
- Claude 原生 `system/api_retry` 映射为 `notice.retrying`，保留运行中的 turn，不当作“已停止”。认证、额度、限流、繁忙与 API 服务异常按结构化类别区分；同一 turn 更新同一条提示，不泄露原始错误体。最终错误只用该结果自己的 HTTP 信息，不继承前一轮重试原因。
- 原生 `error` 字段按 RawMessage 容错，未来版本在非重试帧携带对象不会导致整条 result 被丢弃。未知类别保持中性，不猜账号状态。
- 手机发起 Desktop 导入只能原子领取一次；处理器返回成功只表示已请求打开，不证明目标聊天实际可见。超时或结果不明不自动重复导入，不降级启动 CLI。

专项检查矩阵：

| 链路 | 当前处理 | 验证边界 |
| --- | --- | --- |
| API 选择 / 缺失 / 撤销 | 固定显式 API，拒绝登录 fallback；旧任务不因新的失败请求被停止 | 合成配置与进程；未用真实 Key |
| 模型 / API 切换 | 空闲退休与发送准入互斥，保留原会话 ID；目录不等于模型实际能力证明 | 并发测试及既有 launcher / gateway 回归 |
| 新建 / 续聊 / 继续 | 描述、来源、会话归属、终态 ID 逐层检查 | 不接管其他进程或未知历史 |
| 停止 / 异常退出 | 精确 turn 约束；未确认 OS 退出不开放替代 worker | Windows 合成子进程；Linux 编译检查 |
| 多条审批 / 表单 | 动态待处理数、原生 decisions、逐请求取消/期限 | URL OAuth 与强制本地交互不冒充支持 |
| 限流 / 重试 / 丢回执 | 重试不是终态；已尝试操作继续核对同 UUID | 不把断网变成自动重放新任务 |
| 子任务 / 结果流 | 来源、父子归属、UUID、worker 代际与单调状态版本 | 新版本完整子代理转发仍需单独适配 |
| Desktop Code 入口 | 明示 Bridge 托管 Claude Code 执行器 | 不是控制原生 Desktop 的同一个 worker |
| Desktop Chat / Cowork | API 模式历史只读，写入拒绝 | 尚未实现原生远程发送/停止 |

## 3. 主要文件

| 范围 | 文件 |
| --- | --- |
| 公共契约/注册表 | `internal/protocol/protocol.go`、`internal/agents/util.go` |
| Claude 权限与请求取消 | `internal/agents/claude_permission.go`、`claude_permission_lease.go`、`question_claude_elicitation.go`、`internal/mcpapproval/mcpapproval.go` |
| Claude 状态/发送/退休 | `internal/agents/claude.go`、`claude_map.go`、`claude_turn_controls.go` |
| Claude API 绑定/异常 | `claude_runtime_settings*.go`、`claude_api_conditions.go`、`claude_process_lifecycle.go` |
| 执行描述/Desktop 导入 | `internal/hubclient/agent_status.go`、`dispatch.go`、`internal/agents/claude.go` |
| Codex 原生批准映射 | `internal/agents/codex_requests.go` |
| Bridge 传输预算 | `internal/hubclient/event_budget.go` |
| 手机公共描述/投影 | `src/remote/client.ts`、`approvals.ts`、`projection.ts`、`turn-control.ts`、`api-admission.ts`、`store.ts` |
| 手机队列/草稿 | `ThreadView.tsx`、`ApprovalQueueBar.tsx`、`QuestionCard.tsx`、`QuestionDock.tsx`、`thread-rows.tsx` |

该工作区原有大量未提交修改；本记录不是把所有 dirty 文件归于本轮。

## 4. 验证状态

以下为本轮候选的自动化验证及前序专项记录。合成测试通过不等于真实桌面/手机/上游 API 已完成端到端验收。

- 手机最终 API-only 候选：`npm run typecheck` 通过；`npm test -- --silent` **103 套 / 1,689 项通过，8 套 / 8 项既有跳过**（151.566 秒）。包含动态审批、发送/继续、API 变更、原 UUID 回执恢复、草稿保留、Desktop 执行描述及模型选择回归。
- Bridge 最终 API-only 候选：`go test ./... -count=1 -timeout=240s` 全部通过；agents 153.115 秒、hubclient 42.863 秒、console 60.778 秒、launcher 15.427 秒、gateway 1.156 秒、protocol 0.357 秒、mcpapproval 1.758 秒，其他包及 e2e 均通过。
- Claude 新 API/进程/审批/继续/发送/重试/Desktop 路由定向 race 通过：agents 37.727 秒、hubclient 2.063 秒。最后追加的 API-only/启动撤销归属清理回归 race 连跑 3 次通过（1.803 秒）。
- Linux/amd64 agents 测试包交叉编译通过；Unix 信号终止测试未在该系统实际运行，不记为运行通过。Windows 私有目录 ACL 与合成子进程退出测试已运行。
- `go vet ./...` 通过；桌面/手机仓库 `git -c core.safecrlf=false diff --check` 通过。没有版本升级、提交、发布、部署或真实上游调用。
- Bridge 路由/协议/MCP：`go test -race ./internal/hubclient ./internal/protocol ./internal/mcpapproval -count=1 -timeout=180s` 通过，分别 34.379 / 1.271 / 12.191 秒。
- 新增 `unified_approval_contract_test.go`：117 个叶子用例，Codex / Claude / 合成扩展 Agent 在 open / restricted 策略下经过配对、armed、execution-source guard 及 `Dispatch` 到精确 responder。覆盖一次授权、拒绝、问题、重复响应、错误 session / source / API / controlSurface、能力撤销及解绑；独立普通与 race 运行通过（6.789 / 5.968 秒）。这不是绕开远程鉴权的 local Dispatch 测试。
- Hub：无生产改动；`go test ./internal/hub -run 'Receipt|Command|Event|Source|Pair' -count=1 -timeout=120s` 通过（4.940 秒）。
- 独立复核发现的 nullable Codex options、MCP 大行不换行卡住取消、网络审批上下文遗漏、问题仅支持 deny 却被禁用均已修；注册表到期/撤销路径 race 连跑 10 次通过。
- 最后独立复核补出并修正了旧 Claude 审批回调更新同名新 worker 的状态竞态；新增 `claude_approval_status_cas_test.go`，普通定向 3.411 秒、race 连跑三轮 12.767 秒通过。新 context / worker 退休 / reader 代际各组也单独做了重复 race 测试。
- 测试过程中曾发现原生 Desktop 错误码被通用 guard 覆盖，已修检查顺序并补 deny-only Desktop 测试。Claude 旧合成 fixture 曾错误依赖 helper 自动授权或清除测试 LocalPort，已改为实际捕获 prompt 后明确批准；失败运行不计入通过记录。
- 纯 API 收紧后，部分仅测发送/竞态的旧 fixture 还用空 Settings，手机旧正例还使用 `source: tool` 或非法占位句柄；已改成明确的合成 API，保留全部原竞态断言与无 API 负例。没有通过给生产代码加测试豁免解决失败。
- 两个独立子代理分别完成执行描述/底层边界和手机消费链复核。最后只读复核没有发现这轮候选中的可复现新 P1/P2；这不是无漏洞保证或厂商兼容认证。
- 所有新增模型/授权测试使用合成运行时、临时目录、httptest；没有读取真实账号文件或发送模型请求。
- 不等同于真实 Windows Claude Desktop / 独立 CLI / 手机 APK 的端到端验收；没有本轮真机截图或视觉验收承诺。

## 5. 保留边界与后续事项

1. Claude Desktop 原生 Chat/Cowork 当前仍只读；Desktop Code 来源下使用 Bridge 托管 CLI 的远程能力，不可谎称控制同一个原生 Desktop worker。这是 Salcara 适配边界，不是 Claude Desktop 不支持 API。
2. 不支持的 SDK 权限建议、requiresUserInteraction MCP、URL OAuth 不能用旧 prompt helper 假装支持；强制请求在手机允许也不代表宿主会执行，需原生工具结果确认。
3. 普通批准与问答的持久投递 UI 尚未完全合并；已选动作不能因网络未知自动换 UUID 重新授权。
4. 无 origin / UUID 的旧 CLI 只能采用其原有根流顺序保证，无法按未知关联字段提供不存在的精确证明。新模型目录不等于模型对工具、图片、effort 的权限证明。
5. 子代理 forward-subagent-text、Task 系列待办、URL elicitation、完整附件类型与原生 Desktop 控制仍需单独版本化适配；不因通用接口已建立就扩大能力声明。
6. 长期离线仍受原始审批期限约束；重连快照不复活过期授权，不自动接管未证明归属的进程。
7. Claude 的显式 `auto_all` 现在使用原生 `--permission-mode bypassPermissions`，但只有电脑 `AllowPhoneAutoAll` 已允许且手机明确选择才能进入。单纯继承电脑默认 `auto_all` 或恢复一个未记录原权限的历史不能启用 bypass，默认回到 `ask`。原生仍发出的强制交互、问题和表单仍逐项回答，不以 helper 自动放行模拟原生模式，也不覆盖组织禁止 bypass 的策略。
8. `--settings` 不覆盖组织 managed policy；已经由组织配置的 Claude apps gateway 也不是普通个人 API 设置。本轮不支持此种登录/组织网关环境，没有实现原生最终 provider 的独立证明，不能声称在这种机器上仍能确保 API 来源。
9. Bridge 或系统崩溃可能留下带私有 ACL 的临时 API 配置；正常退出/启动失败均清理，但本轮没有增加可能误删活进程配置的启动全盘清扫。重启后的进程租约不同，不授予遗留 worker 新任务控制权。

## 6. 契约依据

- [Codex App Server 审批](https://learn.chatgpt.com/docs/app-server#approvals)：原生 decisions、resolved 与执行完成区别、网络审批上下文；本轮按 OpenAI Docs 核实。
- 本地已生成的 `work/conversation-protocol-schema/v2/CommandExecutionRequestApprovalParams.ts` 明确 availableDecisions nullable，作为补充版本证据（生成文件未修改）。
- [Claude CLI](https://code.claude.com/docs/en/cli-reference)、[Claude 权限](https://code.claude.com/docs/en/agent-sdk/permissions)、[Claude result](https://code.claude.com/docs/en/agent-sdk/python#resultmessage)：CLI 与 SDK 不混称。
- [MCP cancellation](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation)：逐请求取消与迟到结果处理。
- [Claude 环境变量优先级](https://code.claude.com/docs/en/env-vars#precedence)、[配置优先级](https://code.claude.com/docs/en/settings#settings-precedence)：用户/项目 env 可覆盖 shell；组织策略优先级更高。
- [Claude 程序化运行](https://code.claude.com/docs/en/headless#handle-api-retries)：结构化重试事件与版本化子代理输出，不用网页聊天行为猜 CLI 协议。
- [Claude Desktop 网关配置](https://code.claude.com/docs/en/llm-gateway-connect#desktop-app)、[Desktop 第三方模式](https://claude.com/docs/third-party/claude-desktop/overview)：Desktop 自己的 API 配置不同于 CLI 环境变量；其官方 Remote Control 在网关模式不可用，不代表 Salcara 的自有托管 CLI 链路不能工作。

## 7. Claude 原生权限切换补齐（2026-10-13）

- 契约依据：[Claude permission modes](https://code.claude.com/docs/en/permission-modes)、[CLI reference](https://code.claude.com/docs/en/cli-reference)、[Elicitation hook](https://code.claude.com/docs/en/hooks#elicitation)、[Remote Control](https://code.claude.com/docs/en/remote-control)。`-p` 支持原生 bypass 参数，但 managed policy、必须交互的工具和具体宿主版本仍可能拒绝；不改任何用户/组织授权配置来绕过它。
- 保留三个不同层次：`approvalModes` 为 Claude v1 会话内模式切换能力声明；命令 `approval` 是手机显式下一轮选择；`session.approvalMode` 是实际 Bridge worker 的启动策略。它们不占用或伪装成 Codex `permissionModes` / `permissionMode`。
- `ask → default`、`auto_edits → acceptEdits`、显式且获电脑许可的 `auto_all → bypassPermissions`。已有对话模式改变仅在 worker 空闲时 `--resume` 同一 session；活跃任务、子任务或未决审批不会被切换操作停止。
- 启动锁之后、进程启动之前、写入下一轮之前均重新检查当前电脑许可。撤权只拒绝新准入，不撤销已准入任务。显式 `ask` 可以从原 bypass worker 安全降级；最终写入还比对目标 worker 的策略，防替换竞态把输入送给错误权限的进程。
- 手机仅对声明能力的 Claude Code / Claude Desktop Code worker 显示会话内模式选择；旧 Bridge 不会收到其无法执行的新字段。原生 Desktop 和其他 Agent 不接受 Claude 的审批字段。保留原风格与现有选择器，不新增布局或动画。
- 选择随消息/排队草稿/投递回执冻结。已尝试发送但确认丢失时，可用原 UUID 与原 mode 恢复回执，即使当前授权撤销；不会改权限、换 Key、换 UUID 或启动新任务。服务端若没有已完成回执，仍按当前授权重新拒绝未准入任务。
- 普通审批、AskUserQuestion、MCP form 继续共用动态队列及逐请求生命周期。任一项取消、超时、worker 退出不会替其他项授权；URL OAuth 与不支持 schema 安全拒绝，不返回伪造完成。
- Claude Desktop API 模式的 Code 会话由实际 CLI worker 执行；Chat/Cowork 保持原生历史只读。已核实官方 Remote Control 需要合格的 claude.ai 登录且不支持自定义 `ANTHROPIC_BASE_URL`，因此不能偷偷改用登录方式当作 API-only 原生控制方案。本轮没有真实 Desktop/手机真机端到端验收。

新增回归包括 native mode 参数、同会话切换与强制提示保留、忙碌拒绝、最终 worker 策略比对、启动前撤权、默认不提权、能力声明与真实分支、双端下一轮选择、跨 Agent 拒绝、存储中途撤权、同 UUID exact-payload 恢复、队列/回执重启恢复。最终测试结果由本轮集成验证记录补充。
