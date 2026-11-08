# 多 Agent 聚合内核修复与验证

2026-10-12 的 API 模型编辑、自动同步、覆盖开关恢复与启动事务继续修复见 [模型聚合闭环记录](MODEL-AGGREGATION-CLOSURE-20261012.md)。本页旧测试数字属于历史候选。

本轮只处理适配器身份、真实能力、会话/事件归属、模型选择与跨站恢复的聚合缺口，不修改普通桌面或手机 UI/动效，也不提交、发布或部署。

10 月 11 日的继续实施还补齐共享凭据并发/冷却、真实项目 provider 校验、进程级临时 API 认证、新任务持久回执与普通图片草稿；新增修复和最终候选验证见 [远程内核可靠性记录](REMOTE-KERNEL-RELIABILITY-20261011.md)。下文旧专项数字是历史记录，不替代新候选全量。

完整修复项、文件归属、手机/Bridge 验证和未实现范围见 [聚合执行链路审查记录](../../../../salcara-mobile/docs/maintainers/MULTI-AGENT-AGGREGATION-20261011.md)。

重点：共享 `APIFamily` 只表示 API 选择共用，绝不是工具执行别名；未知扩展没有 builtin 兼容权限；实时扩展必须绑定受核验的 Sink；Codex 原生与 CLI、Claude Code 与 Desktop 只读历史不能互相替换。合成 Hermes 测试不是生产 Hermes 接入。

接续复核已补三类内核保护：仅保留两个内置 desktop family 别名，扩展必须显式声明共享；Codex/Claude pending-only 审批也拦截 API 与整机换站；五类 Codex 审批按原连接生命周期隔离，关闭源不能被新连接回答或占用 busy。手机侧栏另补首批/下一页手动重试及当前 Agent 首读，未重做布局或动效。定向 race/vet 和手机相关测试已通过，准确数字与冻结候选全量记录在上面的统一交接文件；专项结果不代替整仓或真机验收。

账户/额度池/账本和本机 apply 租约由协调任务负责，最终全量须等两侧源码冻结后统一执行，不沿用修复前测试结果。没有真实凭据、真机或付费模型验收。

## 2026-10-13：能力声明与真实执行链闭环

本次只修 Bridge 内核与测试；手机投影修复由同轮主任务处理，普通界面/动效没有在这里改动。当前代码未提交、发布或部署。

- 新增 `internal/hubclient/remote_capabilities.go`，把声明能力与实际 Go 执行接口取交集；`agentStatus` 和远程 `requireAgentCapability` 使用同一结果。适配器不能只声明 `continue`、精确停止、问题回答、图片或模型覆盖，就让手机显示实际无法执行的操作。
- 精确停止必须提供 `PreciseInterrupter`，继续必须提供 `TerminalContinuer`。两条分支原本已经存在，本次不是新造这些能力；缺少接口时现在会在模型目录探测、附件处理等副作用之前拒绝。继续仍要求终止轮次与操作 ID，不等于恢复 Goal，也不改成重新创建任务。
- 正式远程审批/问题必须提供会话范围 responder；历史本机控制台 ID-only 回答有显式 `localApproval/localQuestions` 兼容分支，不能被已武装的远程请求选中。问题依赖批准能力，避免手机可显示问答却无法回答。
- 对话中换模要求 `SendWithOptions`；旧适配器 mandatory `Start(model)` 通过独立 `startModelOverride` 保持合法的新任务行为，但不据此宣称会话内换模。图片输入要求声明与 start/send 图片选项接口同时成立。
- Claude Desktop 公共 Code 卡片的 `attachment.put` 明确路由到 `claude` worker，随后仍发送至原 `claude:<session>`。拒绝 native/read-only 控制面、错误 tool、非法字段及仅共享 API family 的错误别名，不会把 Codex Desktop 变成 CLI，也不会把 Claude Chat/Cowork 只读历史变成可发送对话。
- 原生 Codex 的批准能力还必须有实际 `NativeRespondApproval` 实现；缺少时同时收回卡片、NativeConnection、DesktopLive 三处声明。原生停止/附件本来就被 `ValidateNativeConnection` 拒绝，没有伪造接口支持。
- `session.steer` 没有 dispatcher，因此始终不发布；三个 Codex 权限档只发布给真实 Codex options 接口，不套用在 Claude 或扩展工具。

### 权限撤销与旧默认模式

`session.start` 现在只有省略 `approval` 才能继承电脑默认；显式空值、未知值、错误类型直接拒绝。解析出实际模式后再校验 `AllowPhoneAutoAll`，修掉默认 `auto_all` 绕过手机开关的问题。legacy `auto_all` 同样生成动态同意检查，在所有目录/模型等待之后、调用适配器之前重新检查，并传递到适配器最后提交边界。

协作修复同时在 Claude worker 增加真实策略查询和 Start/Send/writeTurn 的动态同意检查：已有 `auto_all` worker、恢复时的默认策略、继续操作都不能绕过撤权。检查位于图片读取与写锁排队之后、预留/发布新轮次之前；回调不持有 Agent 状态锁，以免查询自己时死锁。拒绝新轮次不会杀掉原来已接受的任务。Claude Desktop API 模式继续用 Claude Code 的 Code 会话执行链；这不是 Claude 账号反代，也不等于官方 Desktop Chat/Cowork 实时输入能力。

### 合成验证与测试隔离

- 全量 `go test ./internal/hubclient -count=1 -timeout=180s`：通过（31.898 秒）。
- capability / approval / 上传 / 模型切换专项 `go test -race ./internal/hubclient`：通过（6.889 秒）。
- `go vet ./internal/hubclient`、本轮文件 `gofmt`、已有跟踪文件 `git diff --check`：通过。
- 回归覆盖真实内置 Agent 构造器及接口（临时状态/历史目录、不启动真实工具）、缺失 optional 方法提前拒绝、Claude Desktop 上传并传至 Code 原会话、错误执行面、只读历史、native 缺批准实现、默认审批/非法值、模型等待期间撤权、传递到两个适配器的 live guard。
- 更新旧模型目录/切站 mock 的 `SendWithOptions` 契约；没有通过放宽生产能力规则让旧 mock 通过。旧本机调用与新任务 mandatory `Start(model)` 兼容用例仍通过。
- 两名执行者进行了分离只读交叉审查，当前候选未发现新增阻断；这些结果不代替根任务最终集成全量，也不等于真实账号、手机或原生宿主验收。

测试隔离修正：首次新增上传回归沿用了旧 `newAttachmentStore()` fixture，发现它会加载真实应用目录中的 attachment bindings，并写入一个 synthetic 会话关联。本轮已只移除能明确归属于该次测试的 synthetic 关联，保留原文件和其他记录；随后 `attachments_test.go`、`image_capabilities_test.go`、新增能力测试统一采用纯临时 `testAttachmentStore`，上面的最终验证均在 fixture 隔离后执行。未读取真实聊天正文或凭据，也未调用付费模型；不能把隔离前的测试描述成从未接触应用目录。
