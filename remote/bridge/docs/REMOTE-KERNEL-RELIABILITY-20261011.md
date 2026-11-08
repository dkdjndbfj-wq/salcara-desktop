# 远程编程与多 Agent 内核可靠性修复（2026-10-11）

这是实际实施和回归记录，不是仅列出待办的交接。保留原有 desktop/mobile 视觉、布局和动效；修复发送、恢复、进程和共享凭据边界。未提交、发布或部署，不读取真实 Key/auth.json、实际聊天或服务器配置，不请求付费模型。

## 已实施

- 共享同一 API 凭据的卡片不再按卡片 ID 分别计算并发和冷却。池与固定来源共用 `AccountPool.AdmitFixed` 的凭据预算；不同 group/工具不能绕过它。保留 priority、归一化负载、weight 和最久未使用轮转。旧 Key 的迟到失败/释放不能修改新 Key，成功不能清除并发中的 429 冷却。状态有明确容量与淘汰上限。不同 URL 挂载、域名别名或不同受保护引用不能无证据推断为同一账号；跨软件 refresh token 仲裁仍不承诺。
- Codex builtin OpenAI 模式不再依赖 app-server 未采用的 `OPENAI_API_KEY` 环境变量。先校验实际 `config/read`，确认进程内 `ephemeral` 存储与 `forced_login_method=api`，再对自有进程完成 `account/login/start` 和 `account/read`。无真实上游请求，不覆盖原登录。管理进程清除可能覆盖来源的 workload/access-token 环境变量；原始用户模式不被这条路径接管。
- 任务实际 cwd 在 start、resume、send 前分别读取有效 provider 配置；项目层旧 URL、认证 headers、reserved provider 和 credential store 不得覆盖所选来源。模型目录、JWT 或运行进程存在都不是远程来源权限证明。
- 自有 Codex 进程强制停止时保存/关闭自有管道，先停止进程再关闭 stdin；正常输出不提前截断。传输退休与实际 OS exit 分开，OS 未确认时所有后续重启被围栏拦截，实际 Wait/Exited 后才解围。未宣称杀尽任意脱离父进程的 Windows 子进程。
- 已写入的 mutating RPC 在 timeout/cancel、partial/short write、进程崩溃、关管道或损坏结果时返回 typed `command_delivery_uncertain`；取消仍可 `errors.Is` 检测。只读请求维持普通错误；写锁等待中已过期的调用不再写进工具。首次创建后的后续发送/附件绑定失败不能当作绝对未创建。
- Claude 自有 worker 的 inherited pipe、transport/OS 退出围栏与旧代审批 helper 一并修复。MCP 权限不再拿全局 token 加 session ID 附着下一代 worker，而是独立 worker token 和 immutable lease。已退休 helper 不得回答新代问题或占用 busy；不会自动终止正常任务。
- 手机普通文字/图片草稿按精确配对和 Agent 入口保存。Claude Code/Desktop 不共用 `new:claude` 草稿；读错、future 格式与丢图不视作空数据或降级纯文本。跨重挂载 CAS 和 accepted tombstone 保留新编辑，未发送图片文件使用私有有界缓存。
- 新任务增加持久 operation outbox：第一个 await 前复制完整提交，先落回执再分块上传，固定 UUID、工具、API、模型/强度、cwd、审批、图片 ID/摘要与原 composer revision。重启后仅明确重试原 UUID，不自动创建新任务；缺图或快照不一致拒绝发送。内部 canonical JSON 拒绝 duplicate/escaped duplicate keys，未知 receipt/source/options 字段保留原盘且拒绝覆盖。
- 有效新任务接受结果先保存 `acceptedSessionKey`；手机本地清理失败仍能打开已创建任务。原 composer 的精确版本消费后才能本地 ACK；换 UI owner 时保留原配对的接受记录，不投影到新站；配对退休之后迟到接受不得变成当前成功。设备换站/API 阻塞区分 unknown 发送与仅本地已接受清理，不让已完成任务永久锁死来源。
- 扩展 Agent 与内置工具复用真实会话 namespace 检查，保留大小写、数字与点等合法注册 ID；拒绝空白/控制字符和虚拟新任务 outbox 哨兵。accepted 结果在落盘前同时核对原 canonical 工具，错误回包不污染全局回执表，也不打开虚拟会话。
- Codex 初始化先登记自有进程租约与 WaitGroup，再发布连接；关闭/调用取消不得漏掉尚未发布的 worker，所有 watcher/request admission 都经过同一个 closed gate。非合作本地 sink 的 Close 等待有界，不把超时当作真实 OS exit。
- Claude 在尝试写输入帧前先登记回合和发出本地 user/started；不持进程 mutex 调用 sink、不阻塞 stdout reader。极快 assistant/result 不再提前完成后又被置回 running，部分写失败仍返回 typed 不确定送达并退役自有 worker。
- 本地 user/started/running 通知阻塞期间进程退出的三阶段围栏已继续补齐：每一阶段与临写入前核对 exact worker，更新 running 先做当前代 CAS；已退休不能恢复 live cache、向同名替代 worker 写帧或当作正常发送。未尝试写入时明确拒绝，实际写入之后仍保留不确定送达；原有 worker/sink mutex 顺序不倒置。
- FIFO 自动推进和新任务的最后发送检查遵守实际 AppState；后台不会主动发出尚未下发任务，但不停止电脑已经接受的任务。
- 明确的 `content.text.codexDelegation.input` 在 CLI/App Server 与原生历史读取中保持相同正文与委派类别；不再显示 native wrapper 并误当普通用户消息。仅保留有界 input 与 `Kind: delegation`，不暴露或推断来源任务/ChatGPT 身份，不增加转发权限。
- 流式节流器按事件身份串行化 partial/final sink，并在终态前等待已在途的局部事件，杜绝“完成后又插入旧片段”的竞态；对从未发送终态的异常流增加 4096 条有界淘汰，避免长期运行内存增长。新增终态顺序与有界状态回归。
- Hub 的账户事件序号使用数据目录内独立的受保护 `event-sequence` 高水位文件，按块预留后才发布事件；崩溃、同毫秒重启或系统时钟回拨不会复用旧游标。只落盘数字高水位，不落盘事件正文，设备凭据仍只保存哈希。无持久化目录的临时 Hub 明确保持进程内序号语义。
- Windows 凭据库锁直接使用 `CreateFile` 返回的整数句柄交给 `os.NewFile`，移除不必要的 `unsafe.Pointer` 转换；`go vet` 不再留下句柄转换告警。

## 文件归属

- Bridge 共享凭据：`internal/gateway/account_pool.go` 的 `AdmitFixed` 及预算/状态、`fixed_credential_budget_test.go`；其后策略/备用层与 gateway 调用接线由已授权协调任务继续实施，完整候选须共同验证。
- Codex：`internal/agents/codex.go`、`codex_rpc.go`、`codex_provider_guard.go`、`codex_start_lifecycle.go`，临时认证/实际 cwd provider、RPC delivery、OS exit、初始化/关闭与 delegation 投影的合成回归；CLI/原生共同消息投影另涉及 `codex_map.go` 和 `codex_delegation_projection_test.go`。
- Claude：`internal/agents/claude.go`、进程生命周期、权限 helper/worker lease、`question_claude_elicitation.go` 和 `internal/mcpapproval`，对应 inherited pipe/代际权限/typed delivery/fast-result 回归。都只管理 Bridge 自有 worker。
- 新任务电脑回执：`internal/hubclient/dispatch.go`、`new_task_receipt.go` 及测试；保留首轮 client UUID，创建后失败不伪装绝对未执行。
- 手机：`src/remote/store.ts`、`delivery.ts`、`new-task.ts`、`client.ts`、`projection.ts`；草稿与接线详见 [图片草稿记录](../../../../salcara-mobile/docs/maintainers/REMOTE-COMPOSER-PERSISTENCE-20261011.md)。新增 `remote-new-task-outbox.test.ts` 与独立 `remote-new-task-safety-review.test.ts`，既有 API/聚合/离线/回执测试修正为合法 synthetic fixture。
- 本记录和手机能力/聚合表只更新实际状态；不删除历史来源文件、不覆盖其它用户修改。其它反代 UI、账号、用量及策略实现由协调任务的独立记录审查。

## 回归依据

全部为合成配对、账号、文件、RPC、HTTP/TLS 与自有辅助进程，真实 Codex 探针使用独立临时 CODEX_HOME 与合成 Key，只调用 initialize/config/read/account 相关接口。

独立检查实际复现了图片对象在 await 中被修改、恢复图片与 canonical 快照不一致、future 回执字段被抹掉、duplicate canonical 字段、旧 owner 接受后丢回执、退休后迟到接受被当成功、扩展 namespace 不兼容、虚拟会话被打开及 wrong-tool 结果写坏全局表。20 项独立合成安全回归与新任务/回执专项合计 5 suites / 79 tests 通过（6.973 秒），gated typecheck 通过；不把修复前通过数字当作新代码证明。

## 验证与边界

- 手机最终 gated `npm run typecheck` exit 0；完整 `npm test -- --silent --runInBand` exit 0：96 suites / 1522 tests 通过，8 suites / 8 tests 既有跳过，共 1530 tests。新增 queue handoff、附件 scope/abort 以及 await/生命周期栏栅回归均包含在本次结果内。跳过项不计入通过；摘要不覆盖依赖、原生 APK/Windows 构建或全仓。
- 分离的手机只读终审核对 start/retry/ACK、source/API 末端验证、scope 切换/退休、future schema 和已有会话发送，没有发现新的必修阻塞项。ThreadView 完整专项 157 tests 通过；未新增或重做手机 JSX/styles/动画设计。维护者草稿说明见 [图片草稿记录](../../../../salcara-mobile/docs/maintainers/REMOTE-COMPOSER-PERSISTENCE-20261011.md)。
- Codex startup/Close 的自有初始化、取消、通知 sink barrier 与 request-handler 快照专项普通 28.930 秒、扩展 race 44.380 秒，vet exit 0；源码摘要稳定。Claude fast-result 阶段专项普通 agents 39.472 秒 / mcpapproval 0.306 秒，race 44.148 秒 / 1.480 秒，vet exit 0；随后退休通知三阶段及 successor 追加修复，因此旧 Claude 数字不作为最后候选证明。这些专项不是整仓 Bridge 验收。
- Claude 退休通知三阶段与同名 successor 的追加回归先 RED（0.357 秒），修复后最终相关普通 agents 38.113 秒 / mcpapproval 0.298 秒；最终组合 race（包含 delegation/metadata/history 映射及所有 Claude 生命周期、权限、delivery）agents 43.710 秒 / mcpapproval 1.396 秒 / desktopcompanion 1.584 秒，三包 vet exit 0、gofmt/差异检查通过。最终 `claude.go` SHA256 `2D1EF948182469AA404751B52D3C7CB8A5A7B5C23654920E8314345FB6FFA72B`；`codex_map.go` `4677EFBFF6465FF2F9351450B9789EE5B9155FAEE5EFAC9983717E87CAFBB91B`。新增测试是 `claude_send_retirement_test.go` 和 `codex_delegation_projection_test.go`，Codex startup/RPC 之前的冻结摘要未变。本任务所有生产/测试已冻结；不把这些定向测试替代整仓验证。
- 委派 parser 专项普通 agents 0.408 秒 / desktopcompanion 0.310 秒，race 1.907 秒 / 1.518 秒，两包 vet exit 0，冻结摘要不变；包含 live/history、混合输入、畸形字段、正常 client ID 和隐私回归。
- Hub 完整 `go test ./... -count=1 -timeout=180s` exit 0；完整 `-race -p=1 ./... -count=1 -timeout=720s` exit 0，vet exit 0。最终 race hub 23.859 秒、launcher 4.077 秒、standalone 246.163 秒、standalone-feed 2.130 秒。首次 race 的 240 秒预算在 standalone 的合成 PBKDF2 校验过程中超时；依据 CPU-bound 栈扩大测试时间后重跑全部，未降低生产 600000 次哈希强度、跳过测试或更改 Hub 源码。检查期间和结束时 76 个 Go 源文件摘要相同：`CE10B34F93CEFB53F44F66C9A201EDF0EEAC2CC2E2CE6ECC7C514D4484FF1AFE`；不是完整仓库或镜像摘要。
- 同工作区已授权协调任务完成统一 Bridge 验收：整仓 ordinary exit 0（366.355 秒）、整仓 race exit 0（623.735 秒）；两轮均为 23 个有测试包 / 5 个无测试包，1035 个主测试通过、9 个跳过、0 失败。跳过为 3 个 Windows 条件及 6 个 opt-in，不算真实账号或实机通过。两轮测试前后 453 个 Go/module 源文件完全相同：`77c4b085a0cd01c4a257b1dda303c8afba802065ffb17fc78725bffeb150d3fe`。这是协调任务执行的统一结果，不冒充本任务另外重跑了一遍。
- 整仓 Go 验证期间仅反代 UI 的 2 个 JS 和账本 mjs 回归变化（旧面板错误不能污染新面板）。最终 UI 冻结后控制台 ordinary/race 各 90 个主测试通过、0 失败（26.328 / 37.953 秒）；所有当前资源的 vet/build exit 0（78.133 / 82.537 秒）。仓库 Node 32 个顶层测试通过（31 单项与 6 个 summary 断言，共 37 检查），额外 OAuth/import 10 个 VM 用例通过，JS 语法与差异检查通过。最终 UI/console/vet/build 前后 1189 个源文件完全相同：`3656dca321d9a293e01274c6fb9f8410b1ba9233d5c91687eaf2eed9df0bc9e4`；不拿旧 1188 文件的摘要证明新 web 资源。完整反代/策略/用量结果及限制见 [联合验收记录](PROXY-AGGREGATION-CONTINUATION-20261011.md)。上述 Node/控制台验证不是浏览器视觉或真实账号验收。
- 分离只读 Bridge 聚合/来源终审未发现新的必修阻塞项：准确 executor/session namespace、显式 API family、sibling busy/子任务/pending-only 审批、sourceOnly 初始与提交围栏、issuer 实际 API 同源 HTTPS/no-redirect、凭据发送前后身份复核、原生来源未证明时受限拒绝均保持。`interrupt` 是策略明确保留的安全停止，不赋予远程执行权限；外部软件持有的原始 Key 不在本软件可全局撤销的范围内。
- 本轮新增的同会话 Continue/Stop 使用精确 turn ID、终态 CAS 和幂等 operation ID；Claude Desktop 的第三方/API 配置仍由独立 launcher 负责，原生 Desktop transcript 不被伪装成可写反代会话。Claude Code/Bridge-owned worker 才可执行手机 Continue/Stop。
- 最后复核手机 325 文件摘要仍与全量验收相同，四个关键 Codex/Claude 内核冻结摘要未变化。本轮 kernel/新增文件/文档的空白、冲突与高信号凭据模式检查通过。对整个手机 `src` 强制启用更严格的 Git 空白规则时，另报告既有 `RemoteScreen.tsx` 样式的四处行尾空白（521、522、523、528）；它们不改变语义，按禁止改 UI 的范围保留，不能把这个全目录命令说成 exit 0。没有为格式化无关样式重写用户界面。

本轮修复不是“所有 Codex 功能完整复刻”的承诺。原桌面执行面仍依赖宿主明确授权/实际能力，不能拿 CLI 替代；外部 ChatGPT 云任务接入、原生专属 avatar、完整 Steer/Side Chat/Goal 等未验证控制、临时本地网站代理和新的生产 Agent 适配器仍应按 [能力核对表](../../../../salcara-mobile/docs/maintainers/REMOTE-CONVERSATION-FEATURE-AUDIT-20261011.md) 保持真实状态。真实账号/额度与手机电脑双站弱网验收未在合成测试中完成。

## 2026-10-11 边界补充验证

- Bridge 整仓 `go test ./... -count=1 -timeout=420s`、`go build ./...`、`go vet ./...` 通过；节流器完整 race 回归通过。Windows 句柄不再依赖 `unsafe.Pointer`。
- Hub 整仓普通测试、构建和 vet 通过；`internal/hub` race 通过。事件序号高水位损坏、重启、时钟回拨和同批事件预留均有回归；持久化部署须独占数据目录并备份 `event-sequence`。
- 手机 `npm run typecheck` 通过；全量 Jest 96 suites / 1522 tests 通过，8 suites / 8 tests 跳过；队列 handoff、附件 scope/abort、source-only 受限换站和生命周期围栏追加套件通过。桌面 companion Node 测试 96/96 通过。
- 以上均为合成配对、HTTP/TLS、文件和自有进程测试；未读取真实凭据或调用付费模型，也未替代 Windows/Android 真机、真实 Codex/Claude、真实 Hub issuer 和长时间弱网验收。不能据此宣称所有 Agent 能力已完整复刻。
