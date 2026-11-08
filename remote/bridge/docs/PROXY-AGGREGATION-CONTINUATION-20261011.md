# 反代与聚合继续修复（2026-10-11）

本轮按用户指定 Cockpit 固定提交的实际代码补齐官方账户、授权/导入和本机 API 服务，并继续审查旧授权恢复、启动/恢复失败、聚合审批和输出状态。功能对照及尚未实现项见 [Cockpit 矩阵](COCKPIT-PARITY-REVALIDATION-20261011.md)，聚合实施归属见 [多 Agent 聚合](MULTI-AGENT-AGGREGATION-20261011.md)。

## 当前代码

- Token/JSON/file/batch import：明确 access/refresh 类型、最多 64 条、wrapper/alias 兼容与严格重复/歧义/上限；文件原文传递，preview 不联网。refresh-only 先保护保存，再由唯一 refresher 交换；失败记录可见，不冒充 ready；删除/身份替换的迟到 attach 不恢复旧账户。
- 套餐/额度：显式读取固定 OpenAI usage endpoint，已知 free/go/plus/pro/team/business/enterprise/edu；主次/独立模型窗口、code review、credits、spend control/reset/source/time。0–99 reserve 仅接受当前身份和 credential epoch 的 fresh 观测，未知/失效/不足 fail-closed。请求选择不偷偷联网。
- 本机账号池：支持 auto、random、single_account、额度高/低、套餐高/低、到期临近优先和 custom priority/weight/backup；空策略保留旧调度。策略取已明确应用的来源账号，single 的请求与目录都限制该账号，远程/命名固定账号不继承池。刷新失败、待保存轮换、needs_login 或待替换登录期间，旧额度和套餐不能驱动调度；订阅 hint 只用于明确选择的排序。
- 订阅到期：明确导入字段或 JWT subscription claim 展示原到期、剩余和来源，和 token expiry 分开。JWT 未验签，因此 hint 不建立认证、权限或 quota。无字段保持未知，不将 null 显示成 0。
- OAuth：独立 loopback-only legacy Codex CLI PKCE、随机 state、严格 callback、单次消费、有界 token exchange/禁重定向；立即取消/超时关 listener、重开授权页。完整手工 callback URL 绑定本次 loginID，只解析不请求；终态清除 challenge，跨弹层/会话迟到结果不污染新状态。
- 工具启动：生产全部 relay/official 连接使用工具/来源/apply nonce scoped HMAC Key，进程主 Key 不用于工具调用。profile/备份保存后、Start 之前发布本次授权；保存/启动/最终绑定失败即刻撤销，不靠可写配置文件。切 A→B→A/reapply 不复活旧 Key，重启丢弃全部本机租约。
- 工具恢复：全部路径/外部编辑检查通过后、首份恢复写入前撤销当前授权；write/Start/save 失败和 profile 回滚不恢复旧 Key。持有 launcher mutex 时实时取租约；旧 cleanup 比较完整 revoked binding，不删除后来一次 apply。
- 远程工具独立派生账号/站点/手机身份 Key，固定所选来源，不继承本机池。当前有效 Key 的持有者仍有对应权限；这些围栏不证明操作系统里所有原生进程均由 Salcara 启动。
- API 服务：独立 named Key、停启/轮换/撤销、alias、exact 与 literal prefix allow/deny，deny 优先；可选客户端 namespace 与 HTTP/WS/目录/Key 检查/image tool 使用同一模型解析。models/responses/compact/chat，原生 Chat 多选和 refusal 保留；跨协议完整工具历史/流非流通过明确验证，不能可靠转换的参数拒绝。停止/撤销的写盘失败仍立即关闭本进程权限，旧操作不能撤销新一轮启用或 Key。
- 就绪检查：本机 handler/lease/绑定/客户端 Key/模型政策的只读验证；不会发上游请求，`upstreamVerified=false`。不能替代真实 OAuth/额度/推理验收。
- 持久 usage ledger：真实 upstream token、HTTP/耗时/状态、账号/客户端/route HMAC 匿名化；有界队列/留存/原子写盘、丢弃/保存/读取错误可见、清理/重试。missing usage 为 null，不存 prompt/body/Key/URL/原始错误、不估价。failed 不被后续 complete/DONE 复活；incomplete 可有真实终止用量但状态仍未完成。
- 官方及命名 API 兼容 WS：protected executor 的 HTTP/SSE 适配，loopback/无 Origin/独立 named Key，维护本连接完整 input/output/加密 reasoning、本连接 response id、warmup、逐 turn 模型和撤销围栏。真实内部 turn 只记账一次，失败清 continuation。裸及 /codex-api 挂载下的 /backend-api/codex/models、responses、responses/compact 已接同一模型/HTTP/WS handler。不是原生上游 WS，不转发任意 backend 路径。
- 图片：新增 generations/edits 专用入口及 JSON/有界内存 multipart，protected 同账号或固定 relay；disabled/enabled/images_only、每凭据默认 1/可设 1–16 的有界 FIFO 图片队列。Responses 显式 image tool 解析 namespace/alias 后校验账号及客户端 ACL并共享队列。旧请求不能放宽已收紧的活跃并发上限；已授予时取消会回收槽位；空结果/部分结果/迟到失败不冒充完成，真实 usage 保留。固定官方 quota reserve 在每次执行及 token rotation 后复查所有实际模型窗口。
- 官方 transport：绕过 pinned SDK 默认会跟随重定向的客户端；Runtime 使用私有无代理 transport 拒绝 3xx，SDK refresh 改用 Salcara 有界 legacy refresh connector。测试私有 seam 不进入生产路径，SDK/module cache 和全局 HTTP defaults 均不修改。
- 上游错误：保留合法状态与白名单 model_capacity，固定容量提示，不回显原始 SDK 错误/凭据。SSE 首 payload 之前的空流/401/错误不伪装 200；401 只同账号一次刷新；部分流不重放不换源。本机额度/并发拒绝和模型容量不足不冷却整个凭据，真实 429 仍保留冷却。
- 用量总览：来源版本在 Store 锁内随成功配置更新发布，延迟通知和 A→B→A 不能复活旧查询；缓存隔离 Server、来源 Key/显示元数据及查询序列。缺费用显示未知，部分已知费用明确标注，实际 0 保留。官方/OAuth/非空 CredentialRef 不发送普通站点用量请求；relay 用量响应拒绝重定向、超限、读取失败与 null。

服务、client、账号和路由由 Store 唯一运行期 epoch 管理。读取 body、目录、bootstrap、refresh 和首 payload 等待期间，撤销再恢复不能复活旧请求；WS 连接沿用原授权版本。无关账号/显示元数据不取消原请求，失败的配置保存不假装提交成功。

invalid_grant/身份改变会原子持久清除 refresh token，needs_login；写盘失败本进程进入 save_failed、拒绝再刷新并提供 RetrySave。保存失败后强制终止不能承诺未保存清理跨重启生效。

## 聚合补漏

API 库与已注册 Agent descriptor/API family 分开；精确工具路由、独立 API 选择、能力/模型 override 门禁、实际 surface 执行归属、会话/事件/media/close 隔离已经实现。`codex-desktop`/`claude-desktop` 只归一化内置 alias，任意扩展 `hermes-desktop` 不因后缀被合并。未知扩展 gateway 仍明确拒绝，不把通用 setter 当作完整生产执行器。

待答 approval/MCP 与运行 turn/subtask 共同阻塞 API/整站切换。Codex 旧连接的审批绑定原 conn.done；旧 request/response 不能借新连接身份继续，连接关闭会取消所属 pending。跨任务发送、容量提示和不同 surface 对话来自此前对应专项，不用一套同名 raw ID 合并来源。

手机复用原布局，审批 badge 按 device/session/raw approval ID 隔离，侧栏初页加载失败可以从已有 footer 重试。原对话最终类型检查通过；完整 Jest 为 94 suites / 1510 tests 通过，8 suites / 8 tests 跳过，合计 1518 tests。src/modules 的 325 文件前后摘要稳定：`F75CBA831DBEFBE2A5A101785F194F338671A6D31A0690719318F94A014E01AB`。

跨任务明确的结构化 delegation 输入在 CLI live/history 与 desktop companion 中保留相同文本、来源标记及原顺序；不把普通文本猜成 delegation。Claude 发送与退出/退休的三阶段竞态已修复：通知回调不持 worker mutex，退休 worker 不能重置 running 或继续写，新同名 worker 不受旧退出事件影响。原对话三个相关包定向 race/vet 通过；整仓结果以下面的最终候选为准。

## 验证范围

本轮均为 synthetic 凭据、本机 provider/工具 fixture，没有读取真实 auth.json/Key，没有真实 OAuth、usage 或付费推理，没有发布/部署。

最终源码已冻结，验收结果：

- Bridge 整仓普通测试 exit 0，366.355 秒；整仓 race exit 0，623.735 秒。两轮均为 23 个有测试包、5 个无测试包，1035 个主测试通过、9 个跳过、0 失败；子用例不重复计入主测试数。
- 9 个跳过为 3 个 Windows 平台条件（Unix shebang E2E、Unix 文件权限、macOS bundle）和 6 个显式 opt-in（两个隔离原生 Codex 配置探测、旧 SDK synthetic callback、实际 Windows terminal、只读运行进程发现、实际 Codex offline catalog decoder）。这些不计为实际链路通过。
- 全量测试开始后仅两个 UI 文件和新增账本脚本改变。453 个 Go/go.mod/go.sum 源码在整仓测试前后完全一致，SHA256 `77c4b085a0cd01c4a257b1dda303c8afba802065ffb17fc78725bffeb150d3fe`。最终 UI 冻结后另跑控制台普通/race，两轮各 90 个主测试、0 失败，分别 26.328/37.953 秒；当前全部资源的 go vet/build 均 exit 0，78.133/82.537 秒。
- 最终 1189 个源码文件在 UI、控制台及当前资源构建验收前后摘要一致：`3656dca321d9a293e01274c6fb9f8410b1ba9233d5c91687eaf2eed9df0bc9e4`。第一轮 1188 文件/`69a28e24...` 为前端修正前候选，不作最终资源证明。历史 1038 文件/717 主测试也不作本轮证明。
- 前端账号 11 项、图片 12 项、账本 8 项、用量 6 个断言场景均通过；node:test 把用量脚本计为一个顶层测试，合计 32 顶层测试。另有 OAuth/import 10 项 VM 回归通过，全部 JS 语法及 diff 检查通过。覆盖原有设计 class、关闭/切换弹层、保存中改稿、旧账本请求失败隔离、账号删除后禁止自动改绑、namespace/并发验证、文件原文和秘密清除。

UI 只读复核确认新账号、API、池和账本继续使用已有 modal/field/input/btn/restore-row/form-grid/stack 和主题变量；stack 的 12px 间距来自原 app.css。账号卡片和管理列表共用中文状态，未知状态不猜成 ready。上述 VM 和源码检查不是实机视觉验收。

完整 Cockpit 对齐仍有纯代码差距：图片池自动选最少任务成员、原生上游 WS、LAN/WSL、多端口、SQLite 全查询/价表。已知 backend-api 的 models/responses/compact 已接通；其余任意 backend 路径明确拒绝。audio 不在用户指定 handoff 的 HTTP 基准清单中，不能把其缺失和已交付的图片接口混为一项。真实 legacy 官方账号端到端、受限官方可信签发器、原生 provider/启动来源和 Windows/Android 实机链路需要可信接口及实际证据。缺少证据时不能改为虚假的成功状态。
