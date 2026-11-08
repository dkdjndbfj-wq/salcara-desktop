# Claude 远程兼容性调查（2026-10-13）

状态：本文保留“先调查再修”时的审计快照。随后已实施 C01–C05、通用审批描述、动态队列及相邻竞态修复；以 [通用 Agent 链路修复记录](UNIFIED-AGENT-APPROVAL-IMPLEMENTATION-20261013.md) 的逐项状态和实际测试为准。本文旧表中的“未修复”不代表最新状态，也不是发布验收结论。

用户要求先查清 Claude 与 Codex 的差异，再继续修。本次在该要求后冻结新增生产代码和 UI 修改，由三个审查方向分别核对手机交互、Claude Code 执行器、Claude Desktop。没有使用真实账号调用模型，没有发布或部署。本文区分官方文档能力、当前代码实现、推导风险和待真机验证项；不能把 SDK 文档直接当成本项目旧 CLI/MCP 接口的契约。

## 0. 用户明确的产品目标：一个通用 Agent 远程客户端

不是为每个 Agent 重做一款手机应用，也不是让手机直接对接每个厂商。目标架构是：

```text
手机：一套会话、消息、工具、文件、审批、子任务界面
                  │ 统一且版本化的命令 / 事件协议
Hub：配对、权限、投递、回执、补拉；不推断厂商运行语义
                  │
电脑 Bridge：会话身份、能力校验、请求注册表、执行路由
                  ├─ Codex 适配器
                  ├─ Claude Code 适配器
                  ├─ Claude Desktop 原生 / 只读适配器
                  └─ 后续 Agent 适配器
```

现有 `Agent`、`AgentDescriptor`、`SessionInfo`、`Event`、Manager 和 session.* 命令已经是通用骨架，不必推翻。当前还没彻底统一：手机 ThreadView 仍按 Codex 名称决定权限模式、effort 等；desktop 命令和来源兼容逻辑也需逐步归入受控适配，而不是直接删除保护性分支。

最小增量设计，尚未实现：

- 保留基础命令和布尔 capabilities，新增可选、版本化明细：允许的操作、权限选项、审批 decisions 与范围、附件 MIME/大小/数量、停止和继续的真实语义。
- 手机读取经过校验的声明式描述，用既有组件显示；不执行适配器传来的脚本、不加载任意 UI，不把 arbitrary 原生参数透传成权限。
- 同一类请求共享动态审批队列，但每条请求自带真正的动作与范围。未知字段可忽略；未知授权或执行能力必须禁用，不能默认放开。
- 基础内容形成公共模型，Agent 专属信息保留受限扩展，不压扁成纯文本。新 Agent 使用现有内容类型时，主要增加电脑适配器即可；全新交互类型仍可能需要更新手机协议/组件，不能承诺永不升级手机。
- 实际可执行能力是适配器实现、运行时版本、当前会话所有权、站点授权和操作相关模型能力的交集。手机按钮出现不替代电脑端最终检查。
- API/模型聚合回答“向哪个供应商、模型发推理请求”；Agent 聚合回答“控制哪个真实会话、工具和任务”。API family、历史 Client 标签与 ControlSurface 分开，不能因为共用一把 Key 就混用执行器或授权。
- 每个适配器运行同一套契约测试，再叠加厂商专属测试。先覆盖准确性、撤销、重连和幂等，后扩功能；同一公共协议不等于所有 Agent 已有相同功能。

## 1. 首先区分执行面

| 执行面 | 当前 Salcara 实际链路 | 不能混淆的边界 |
| --- | --- | --- |
| Bridge 托管 Claude Code | 独立安装的 `claude -p`，stream-json，Salcara MCP 审批 helper | 不是完整 Agent SDK 控制通道；不是对另一个正在运行的 CLI 窗口发命令 |
| 从 Desktop Code 进入的远程会话 | 当前控制能力仍来自上述 Claude worker，保留 Desktop 来源标记 | 不是正在运行的原生 Desktop worker；配置、版本、MCP 工具和批准结果不保证相同 |
| Desktop Chat / Cowork 历史 | 受限读取，允许 list/open/describe，变更指令拒绝 | 只读是 Salcara 当前适配限制，不是 Claude Desktop 不支持 API |
| 官方 Claude Remote Control | Anthropic 提供的另一条产品链路 | 官方当前不支持 API Key 或自定义 API endpoint，不能拿它替换 Salcara 中转站 API 远程链路 |
| Codex app-server | 已有独立适配 | 可共享消息/审批展示组件，不可复制 Claude 的权限值、控制命令或结束条件 |

Claude Desktop 3P 本身支持 Chat、Cowork、Code；这与“Salcara 是否能控制原生桌面会话”是两个问题。[官方 3P 概览](https://claude.com/docs/third-party/claude-desktop/overview)

官方 Remote Control 对账号和 endpoint 有限制。[官方 Remote Control 前提](https://code.claude.com/docs/en/remote-control#requirements)

代码入口：`internal/agents/claude.go`；`internal/hubclient/agent_status.go`；`internal/hubclient/claude_desktop_history_route.go`。后者禁止把原生只读会话悄悄降级为 CLI 发送。

## 2. 动态审批：不能把 1/4 当固定步骤向导

当前手机数据本来就是动态 pending 集合，但 `ThreadView.tsx` 固定取 `pending[0]`，只显示“还有 N 个”；没有独立的活动请求选择或逐请求草稿集合。一次 AskUserQuestion 内的多个问题，与多个先后到达的审批请求，也不是同一层级。

建议复用已有紧凑卡片、问题停靠层和粒子动效，按以下完整交互契约收敛。并非全部从零实现：当前已有请求 ID / scope 校验、过期失效、问答持久回执/恢复以及提交后退出动效；主要新增缺口是可选择且稳定的活动请求、逐请求草稿、普通批准回执对齐和原生单请求取消：

1. 显示“待处理 N 项”，不是宣称一共只有 N 项。每次收到、撤销、过期或处理请求都重算；若保留 `i/N`，N 仅代表当前集合，不能表达流程完成比例。
2. 用请求身份锁定正在查看的卡片，不用数组下标。新来的请求只增加计数，不能顶掉当前请求、滚动位置、选择或未提交文字。
3. 一次允许/拒绝只消费该请求。处理后进入下一条；新审批可继续进入，队列不是预先封闭的。
4. 审批与问答共享队列，但保持各自字段、答案和允许范围。AskUserQuestion 的问题页数不能被用作审批总数。
5. 每个请求保存自己的草稿；切换卡片不清空。提交中的请求只显示真实投递状态，不算“已批准执行”；网络未知不能复活为可再次随便点击的新请求。
6. 电脑端已处理、宿主取消、worker 更换或请求到期后，立即使旧动作失效。已失效请求的草稿只能作短暂本地恢复提示，不能自动提交到新请求。
7. 不新增默认“全部同意”。记住授权必须展示精确范围，并只使用执行器确实支持的决策；不同工具、路径、服务器、子任务不可自行合并授权。
8. 仍保留用户已要求的提交后退出问题卡、主对话显示投递状态的行为；不为了审批队列把主消息区堆满卡片。

实现建议：请求键包含站点/配对代际、设备、执行面、session、worker 代际、原生 request ID；原生字段不存在时只能标记未知，不能凭标题或文字猜。由电脑的请求注册表和有序事件决定 pending，不能从历史聊天文本重建可执行审批。重连以同一来源的快照校准，旧站点迟到响应不能应用到新站点。

该方案是 Salcara 的设计建议，不是声称 Claude 官方 UI 按此实现。Codex 官方有 request/resolved 生命周期；Claude 的 prompt helper、SDK callback 和 hook 必须各自提供等效的撤销/响应证据。[Codex app-server 审批事件](https://learn.chatgpt.com/docs/app-server#approvals)

## 3. 已确认的高优先级缺口

下面的“确认”指源码存在该处理缺口；除注明已有测试者外，不意味着已经在用户真机复现。

| 编号 | 当前证据 | 影响与下一步要求 |
| --- | --- | --- |
| C01 · 高 | `claudeArgs` 仅在 auto_edits 时传 permission-mode；ask 未显式设置 default | 会继承宿主默认模式，手机“请求批准”不保证实际采用同一策略。应固定明确模式，同时尊重组织/项目 deny、ask 规则，而不是覆盖它们 |
| C02 · 高 | `mcpapproval.Serve` 忽略 notifications/cancelled，`callApprove` 没有逐请求取消 context | 同一 worker 中已撤销的单条审批可能仍在手机显示；迟到 allow_session 还可能写入后续授权。需要原生 request ID → context → HTTP → 注册表贯通取消，处理取消/响应同时到达 |
| C03 · 高 | helper 和 PermissionRequest 仅保留 tool_name/input/tool_use_id；decideForWorker 用工具名缓存 allow_session，并会按 auto_all 自动允许 | 无法证明保留了强制逐次确认的语义。不能把 SDK 的 reason/suggestions/suppressAlwaysAllowRule 当作旧 MCP 接口一定存在的字段。该通道无法表达的长期授权应收窄或不暴露 |
| C04 · 高 | `claudeLine` 没有 origin；`handleLine` 把所有无 ParentTU 的 result 弹出主任务 FIFO 头 | 后台通知、跨任务注入的结果可能错误结束手机提交的主任务。应按真实消息来源及关联标识隔离，不能只按结果出现顺序猜归属 |
| C05 · 高 | `ContinueWithOptions` 检查 expected 后，未把它写入 opts.ExpectedTerminalTurnID 再 SendWithOptions | writeTurn 已有的最终状态比较没有生效；等待期间另一条操作改变终态后仍可能发送。需补实际发送前的原子检查及竞争测试 |

官方权限模式可能继承设置；需要用户交互的请求和显式 ask 有额外规则，不能把 Claude 压成 Codex 三档。[权限评估](https://code.claude.com/docs/en/agent-sdk/permissions)

旧 permission-prompt-tool 还有明确的上游限制：指定需用户交互的 MCP 工具，即使返回 allow，也会被宿主拒绝。因此“手机有允许按钮”不代表该通道能批准它。[CLI prompt-tool 说明](https://code.claude.com/docs/en/cli-reference)

MCP 支持按 requestId 撤销请求，迟到结果需要安全处理。[MCP cancellation](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation)

源码：`internal/mcpapproval/mcpapproval.go`；`internal/agents/claude_permission.go`；`claude.go`；`claude_map.go`；`claude_turn_controls.go`。

## 4. 对话与运行差异矩阵

| 范围 | 当前已做 | 仍需补齐或验证 |
| --- | --- | --- |
| 请求身份/授权范围 | 注册表按会话和 worker 生命周期校验、原子消费、到期拒绝 | Claude ToolUseID 未用于逐请求撤销；approval 事件重建也未保留父任务/turn 关联。手机“本对话都允许”实际是本会话某个 ToolName，可能比用户看到的具体命令宽得多 |
| 多审批/重复消息 | 多请求可独立等待，快照只返回当前有效项 | 活动卡片固定第一条，普通批准与问答回执恢复不统一；需压测重复请求、同时回答、乱序 resolved、单条撤销及计数稳定性 |
| 问答 | AskUserQuestion 多选、自填、答案验证，并按原始题目回填 updatedInput | 选项 preview 未保留；新 schema 不能无条件当普通文本。官方子代理 AskUserQuestion 有限制，不能承诺每个子任务都能提问 |
| MCP 填表/登录 | 有 worker 绑定 HTTP Elicitation hook，受限表单有类型校验；不支持的内容安全拒绝 | URL elicitation 当前直接拒绝；登录授权和工具执行批准必须分离。手机打开 localhost 回调地址不等于完成电脑 OAuth，不能自动宣称已登录 |
| 主任务/子任务/注入消息 | ParentTU 子任务结果不结束主任务，有子任务 lifecycle 解析 | origin、user-message 关联、结果 UUID 去重、conversation reset 和新终态字段未完整覆盖；不能把机器人/其他任务消息标成用户发言 |
| 子任务正文 | 已有 ParentID mapper、历史读取和摘要展示 | 启动没开启 forward-subagent-text，前台子代理文字可能根本没进入流；新 agent_id/嵌套身份还需版本化处理。无证据时保留未知，不能捏造图标/名称 |
| 流式输出 | 已启用 partial messages，合并文本/思考与最终块 | 工具参数流、截断块、重试、rate_limit_event 等覆盖不足；未知事件不应错误转换成“停止”。当前不因 message_stop 就结束整个工具任务，这一点需保留 |
| 真正结束/失败 | 结果 is_error 或非 success 会失败，进程退出有投递不确定处理 | terminal_reason/api_error_status 未解析；应版本兼容地读终态。旧 CLI 的 subtype=success 且非 is_error 可合法完成，不能因缺少新可选字段就一律判未知；反之没有真实结果/退出证明不能推断成功。来源错误会污染发送/停止/继续按钮 |
| 停止 | 精确校验 Bridge 拥有的活动 turn 后停止自有 worker；有子任务或多个待处理 turn 时拒绝 | 是进程停止，不等于官方 SDK interrupt；检查到执行之间竞争仍需专测。不能为了按钮可点去杀整个桌面或其他任务 |
| 继续 | 同会话校验和重复操作占位；符合所有权证明才恢复 | C05 未修；还应明确“恢复原运行”和“同会话新增续写一轮”是不同操作，不能复用 Codex 名称后隐瞒差别 |
| 换模型/换 API | 空闲时重启自有 worker 并 resume 原会话；忙碌/待批/子任务期间拒绝切换；不支持的 effort 明确拒绝 | 不是 SDK 在线 set_model。目录可选不证明模型支持 tool-use、图片、上下文、缓存、effort；不能把默认别名、plan 路由和实际响应模型混为一个值 |
| 计划/待办 | TodoWrite 映射计划；ExitPlanMode 显示工具动作 | TaskCreate/Update/Get/List 暂为普通工具。退出计划可能改变执行权限，不能当普通通用“本对话都允许”；没有待办事件不代表任务没在进行 |
| 历史/恢复/所有权 | Bridge 自有 worker，满足真实完成证明的 transcript 可恢复；外部活动会话不擅自接管 | 证明在内存；Bridge 重启后没有持久的安全接管凭据。恢复历史不代表可继续同一原生 Desktop worker；不得同时写两个运行实例 |
| 长期离线 | 10 分钟审批期限，过期拒绝；快照不延长授权 | 不是无限等待；未接 durable defer。需区分“手机离线但电脑继续运行”和“电脑进程结束后需要恢复”，避免自动重放过期授权 |
| 图片/文件 | 图片传输有绑定、大小/类型限制；每轮最多 4 张 JPEG/PNG/WebP，每张 8 MiB | PDF、普通文档、Artifacts、交互式 MCP App 不等同图片。不能用一个 attachments=true 宣称完整 Desktop 附件能力；保留轻量手机文件阅读和外部浏览器边界 |
| 用量/耗时 | 转发已有 usage 和本地观察时间，不强求每次有统计 | Claude result 的 duration_ms 是顶层，当前结构却读 usage 内字段；cost 为累计估计而 usage 可为单轮主循环，协议未注明口径。当前手机不显示 cost，不能据此断言已发生账单误计；以后不能直接逐轮累加累计估计 |
| 版本/能力 | capability 与实际接口有交集投影，缺执行分支不会再仅凭声明放行 | 接口存在仍不证明特定版本/会话/工具可执行。应按执行面、版本、会话状态和实际连接分别确认；不要增加阻塞页面打开的网络检测 |
| 资源/性能 | 有审批期限、JSON 请求体上限、输出字段截断 | helper 的 ReadBytes 缓冲大小不是单行硬上限；逐调用 goroutine/等待项还缺专门数量上限。应有背压、取消回收与大行测试，不用常驻手机长连接补逻辑漏洞 |

参考文档各自支撑的范围：

- 输入回调、问题答案格式和子代理限制：[Handle approvals and user input](https://code.claude.com/docs/en/agent-sdk/user-input)。
- 流式块与中途截断：[Streaming output](https://code.claude.com/docs/en/agent-sdk/streaming-output)。不能把一条完成块当整轮完成。
- result 的 origin/终态字段与后台通知归属：[Python SDK result 参考](https://code.claude.com/docs/en/agent-sdk/python#resultmessage)。后台通知可独立产生 result，不保证一条 result 对应一条手机输入。
- Task 工具与 TodoWrite 不同：[Todo tracking](https://code.claude.com/docs/en/agent-sdk/todo-tracking)。需要工具返回的 ID 关联创建和更新，不靠标题匹配。
- 长等待 defer、hook 决策：[SDK hooks](https://code.claude.com/docs/en/agent-sdk/hooks)；MCP 表单/URL 区分：[Elicitation hooks](https://code.claude.com/docs/en/hooks#elicitation)。
- 模型别名/权限/推理设置：[Model configuration](https://code.claude.com/docs/en/model-config)。
- 单轮 usage、累计成本和估计限制：[Cost tracking](https://code.claude.com/docs/en/agent-sdk/cost-tracking)。它们不是余额或正式账单。

## 5. Desktop 专属边界

1. Desktop 自带 Claude Code，独立 CLI 是另一份运行时。Desktop Code 还会读取桌面专用 MCP 配置，重名优先级与独立 CLI 有差别。Salcara 只注入自己的审批服务并不能继承所有桌面工具。[Desktop/CLI 对比](https://code.claude.com/docs/en/desktop#use-the-cli-and-desktop-together)
2. 对手机创建且需要导入的 Claude 会话，`desktop.navigate` 当前发 `claude://resume` 并返回 requested；已有原生 Desktop 会话的精确定位仍会拒绝。前者没有宿主回执，不等于窗口已经打开，更不等于所有权移交。需验证失败回退、重复点击、进程已运行和 API 模式。不能用有订阅登录限制的新 CLI 转移命令假装解决所有模式。
3. Chat/Cowork 的原生应用控制、系统屏幕/辅助权限与 Code 工具批准不是同一层。尚未找到并验证可供本项目使用的完整原生控制 RPC；此处保留明确不可执行边界，不自动批准系统权限，也不推断接口永远不存在。
4. 3P Gateway 的模型发现与推理协议分开。当前兼容路由将非原生模型映射到受控别名；需专项验证小上下文模型、图片/工具使用和压缩门槛，不能只验证模型菜单出现。[Gateway 契约](https://claude.com/docs/third-party/claude-desktop/gateway)
5. 手机侧只做轻量内容访问：公网网页外部浏览器打开；电脑 localhost 不是手机 localhost。本地预览如要开放，应使用显式授权、单目标有期限的转发，不把整个电脑端口或目录暴露到 Hub。此条是设计边界，未新增实现。

相关代码：`internal/hubclient/dispatch.go` 的 desktop.navigate；`internal/toolcfg/desktop_3p.go`；`internal/config/claude_catalog.go`；`internal/hubclient/attachments.go`。

## 6. 后续修复顺序与验收门槛

先修准确性和授权，再扩功能，不先重画手机界面：

1. C04 主任务/注入来源隔离、结果去重与明确终态；C05 继续的最终状态检查。
2. C01/C02/C03：明确初始权限、逐请求取消、收窄记忆授权。旧接口能力不够时保留拒绝边界，另设版本化 SDK 控制适配，不把字段硬塞进去。
3. 扩充统一请求描述：kind、scope、来源、允许的 decisions、expiry、运行时版本/代际。前端复用既有组件，仅显示真实支持的操作。
4. 实现动态 pending 队列、稳定活动请求、逐请求草稿和投递状态；沿用现有外观/动效。
5. 补子代理正文、Task 待办、MCP 表单/认证边界、文件类型能力和模型回执。
6. Desktop 原生控制与托管 Code 严格分开；没有原生执行确认就不能验收为桌面投影。

必须加入或补充的合成回归场景：

- 审批从 1 → 4 → 2 → 5 动态变化；当前填写项不跳、不丢草稿；某条 resolved 重复/乱序到达。
- 手机和电脑同时处理；取消先到/答复先到/同时到；旧 worker 请求迟到；同 request 重放不产生新授权。
- 主任务和两个子任务分别提出权限请求；工具同名但路径/服务器不同；要求逐次确认时不得用 remembered grant 通过。
- 超过审批期限、手机断网重连、Bridge 重启、解绑重绑、A/B 站切换；旧请求不跨身份恢复。
- 一个 AskUserQuestion 含多题、多选、自填；与工具审批混排；不支持的 preview/schema/URL 不自动批准。
- 一条手机输入，随后多个 task-notification result（含 num_turns=0）；人类消息、peer 消息、后台通知分别关联。
- 重复 result、未知新终态、success 但 is_error=true、API 重试/容量/限流、截断工具参数、流尾缺失。
- 继续在锁等待期间被其他发送抢先；来源/权限撤销；部分 stdin 写入后失败；不能双发送或伪称未发送。
- 停止主任务时子任务仍活动；停止前主任务刚完成；手机断网不能被当成用户点击停止。
- 换模型/API 时已有待批、背景任务、模型目录变化；实际响应模型与选择不同；不支持的 effort 不静默丢弃。
- TaskCreate 的 ID 由 tool result 到达后建立关联，更新先到、删除、重放、恢复旧历史。
- 独立 CLI 与 Desktop 版本不同、MCP 配置同名冲突、API 模式、Chat/Cowork 只读动作被调用。
- 大行、并发审批上限、取消后资源释放；页面先显示本地内容，探测不能卡住整个页面。

待真机矩阵：Windows 独立 Claude Code；Desktop Code（官方登录/API 3P 分开）；Desktop Chat/Cowork（只读能力与原生授权分开）；最新支持版和最低兼容版。测试通过只覆盖对应执行面，不外推。

## 7. 已完成验证与未覆盖范围

在本次“先调查再修”的冻结点之前，前一阶段已完成：

- 手机 typecheck 通过；全量 101 suites / 1624 tests 通过，8 suites / 8 tests 跳过。
- Bridge agents 全量通过（158.606s），相关权限/生命周期定向 race 通过（4.552s）。
- Bridge hubclient 全量通过（31.898s），相关 capability/permission/upload/model-switch 定向 race 通过（6.889s），vet 通过。
- 本地 API UI 既有五个 Node 测试文件由主审复跑，86/86 通过；其他本地启动验证见 [本地 API 完成记录](LOCAL-API-SIMPLIFIED-20261012.md)。

这些测试不是本报告新增缺口的修复证明，也不是 Claude 真实服务/原生 Desktop 端到端验收。本轮没有靠测试真实模型消耗 token。前阶段一条合成附件 fixture 曾接触真实本机绑定缓存，该精确合成条目已经移除、其他记录保留，fixture 已改临时目录；不能声称整个开发过程从未触及本机缓存。

用户授权协调的另一对话已收到范围消息，但其运行返回历史解密错误，未取得对方新的审查回复；本报告没有把它算作独立复核。本文由当前线程三个方向核查汇总。

冻结后新增的仅为此调查文档；没有为凑齐能力开启不支持的批准、放宽授权、修改产品 UI 或发布更新。
