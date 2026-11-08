# 远程聚合发布前核查 · 2026-10-13—14

## 结论与范围

本轮修复了切站接收、原生派发授权、消息恢复排序、图片缓存配额、Windows 项目目录边界和手机通知生命周期问题，并增加多用户并发隔离回归。

**这是代码核查记录，不是“全部原生能力已对齐”或“可直接商业发布”的声明。** 没有发布、推送、部署真实服务器，没有调用真实付费模型，没有修改桌面/手机界面布局或动画。

工作区原有大量未提交修改。本记录只归属下列增量；账号导入、额度估算和账号池后台刷新由另一协同任务维护，不能将整个工作区的差异都算成本轮工作。

## 正式商业发布裁定（2026-10-14）

**当前 No-Go：不批准以“原生桌面完整远控已完成”的范围正式发布。** 已确认的可复现内核/资源问题按下文修复并验证，但功能边界和真实设备证据仍不满足原有全量承诺。测试数量、编译成功、某一项 race 通过，均不能代替这一裁定。

| 门禁 | 已有证据 | 通过发布所需的剩余条件 |
| --- | --- | --- |
| 换 API / 模型 / 中转站与收发一致性 | 有来源授权、设备级锁、目录版本、固定 UUID、回执/排序/补收及取消竞态回归；手机最新 1,958 项通过 | 用实际版本的 Codex / Claude API 执行器完成双站断网、休眠、重启和旧请求迟到矩阵；保留每轮可核对的请求与回执证据，不读用户私密聊天 |
| 多用户隔离与资源上限 | 32 独立配对隔离；长轮询/App SSE/Bridge SSE 实际活跃计数；阻塞写、反复重连和删后重建先红后绿；Linux 全量 race 通过 | 在目标硬件按计划并发做持续运行、慢消费者、磁盘压力与峰值恢复测试；准入数字不是人数容量承诺 |
| 手机与桌面安装/更新 | 手机/桌面源码回归、签名/目录替换与回滚 fixture、发布身份门禁 | 本轮 APK 原生构建、真机 FCM/相册/分享/手势、覆盖升级保留配对与草稿；真实 Electron A/B 更新与宿主兼容性矩阵 |
| 原生桌面控制 | 安全地关闭不能证明实际 API 来源的原生执行能力；CLI 托管路径独立 | 原生 Codex 实际来源证明及真实可执行协议未解决；Claude Desktop Code 是托管 Claude Code，不是原生同 worker，Chat/Cowork 只读。不得用 CLI 替代来宣称完成 |
| 发布产物可追溯 | 补同 SHA 的 Linux/Windows 检查；Hub 原件复制而非测完重构建；手机精确版本说明 | 新工作流必须在同一冻结提交执行成功，验证签名、包/镜像摘要和实际安装；本轮不触发远程发布流程 |

任何串用户、重复执行、错 API/错站调用、丢失已确认消息、回滚破坏数据，均为阻断项而非可忽略体验问题。不通过删除记录、改 UUID、跳过测试或放松来源验证获得“成功”。跨任务可靠来源/回复、部分原生问答/任务操作等未完成项仍见末节；如要缩小首发范围，应明确更改产品承诺，不能在验收中偷偷当作已做完。

## 用户关注场景

| 场景 | 当前行为与验证 | 仍需注意 |
| --- | --- | --- |
| 换 API | 空闲时切换受管理的远程执行器；检查 Agent、来源授权、会话身份和新 API 的模型目录。忙碌/待批准时拒绝变更，保留原任务。已有专项用例并在全量回归中通过。 | 活跃的原生 Codex 桌面租约不会被伪装成热切 API；该路径明确要求先结束桌面实时连接。 |
| 换模型/强度 | 校验当前 API 的新鲜模型目录与实际执行器能力；迟到的旧目录不能覆盖新来源。不支持旧模型/强度时拒绝，不悄悄换到其他模型。 | 不承诺每个上游都有相同的思考档位；原生能力依赖宿主实际工具 schema。 |
| 同 Key 后台换模型组 | 会话入口刷新、目录版本和来源指纹检查已有回归。 | 没有对真实付费上游发探测请求；供应商自身收费规则不在本轮测试内。 |
| A 站离线，从 B 站接收切站指令 | 修复备用站轮询成功响应被提前读空的问题；检查同一手机绑定身份和切站回执。 | 必须是已配置、已授权的目标站点，不能凭任意 URL 接管电脑。 |
| 重试/短时断网 | 保留请求身份与回执；原生请求已派发后，回环 HTTP 断开不取消已开始的原生调用。 | “执行情况不确定”的回执仍保守禁止盲目重放，不声称不确定请求一定没执行。 |
| 撤绑/授权变化 | Bridge 取消尚在等待的 native admission，Go 的 HTTP 派发前再次检查；Node 在 ledger/catalog 异步阶段结束后也再次检查。 | 取消的线性化边界是实际原生派发；已经接受的任务不会因断网被杀掉。 |
| 消息恢复 | 修复委派消息/子任务重放缺元信息误吞本地待发气泡、误挂图片，以及 final 快照改变消息顺序。 | 内容版本和显示位置分开处理，不靠相同文字猜回执归属。 |
| 图片 | 分片/完成缓存有总体预算；重试不重复计费配额，过期清理，读取有大小和短读检查。 | 不驱逐仍有效的任务图片来容纳新图片；满额会拒绝新图片。 |
| 多人用同一个 Hub | 新增 32 组独立配对、命令、回执、事件和撤绑的并发测试；单用户占满队列不阻塞另一用户。 | 这是共享 Hub、各控各电脑；不是多人同时操控同一台电脑。 |
| 关闭通知/后台切站 | JS 与 Android 共用持久通知开关；注册迟到结果不能恢复已关闭状态，旧站轮询/旧批准按钮不能套用新连接配置。 | Android 原生改动需要重新构建 APK，尚无本轮真机 FCM 验收，不能靠 JS 热更新代替。 |

## 2026-10-14 增量：站点能力与轮次控制

- **Steer 已部分接通**：仅 Bridge 已管理的 Codex CLI 活跃轮次。真实 `turn/steer`、精确 `expectedTurnId`、同 UUID 去重、派发前复核来源/权限、迟到确认不复活任务。手机入口复用现有控件与持久回执，不覆盖模型、强度或权限；普通发送仍为 FIFO。Claude、原生 Codex 桌面未验证此契约，继续关闭。
- 手机普通 API 与官方账号远程分开禁选。新协议要求明确、新鲜、同电脑已配对的目标站；不能借旧单一站点提示绕过能力缺失。API+站点仍一次电脑事务，日志持久化后重新确认能力，原请求恢复保持原始 payload。
- 新 `station.handover.v2` 为纯连接迁移；不经 A 为 B 获取执行 grant。模型列表同样受来源授权，challenge/grant 计入总计 30 秒读预算。策略拒绝不再把正常电脑标离线。
- native 批准与“在电脑打开”分别要求最新的精确 lease/capability；旧 pending hook 或 client 名称不授予操作权。批准卡禁用与提交状态拆开，避免离线或无权限时一直转圈；样式、布局、动效保留。
- 新版多用户隔离及共享地址专项三轮 race 通过（3.845s）；包括 32 独立配对、相同 UUID 并发重试、其他电脑伪回执、消息隔离、撤绑及一人满队列。**不是 32 人持续负载或任意服务器容量承诺。**
- 手机专项 4 套件 / 255 项通过；后续 native gate 回归发现上述假转圈并修复。完整回归先得到 113 套件 / 1,907 项通过、1 项失败；失败为合法 native approval 测试缺少新协议的显式授权声明。补充该正例声明并增加“事件不能自授能力”的反例后，3 套件 / 55 项通过，再次完整回归 114 套件 / 1,909 项通过、8 项跳过（515.065s）。这次全量在下列取消/切站收尾之前，不能代替最终增量验收。协作对话负责新增 Hub policy/Bridge source scope 的整体集成。

### 2026-10-14 后续反例检查（验收仍在进行）

- `respondApproval` 原先只在入口检查取消信号，没有把它传给底层命令。来源 challenge/grant 等待期间离开会话，旧批准仍能继续派发。新增 `remote-approval-cancellation.test.ts` 在 CLI/原生两路径均先复现失败；两处调用传递同一 receipt signal 后，连同 native isolation、question delivery 和 source grant 共 4 套件 / 31 项通过（6.953s）。进一步发现普通允许/拒绝按钮原本没有 controller（只有结构化问答有），已补同一页面生命周期的 controller 和单次请求 ID；真实组件 CLI/原生两反例先失败，修复后相关 4 项通过（49.026s）。普通批准不因此变成自动重试。取消阻止后续网络派发，不声称撤销已被电脑接受的批准；原 UUID/不确定回执恢复规则保留。
- 子代理独立复现并修复三个切站 journal 问题：全新操作中 A 明确未派发、B 也明确拒绝时清理此操作；B 不支持备用通道的发送前失败有独立错误类型；后来一次政策拒绝不能删除旧的不确定 journal。保留原 UUID/载荷，不能盲目换目标重发。先红后绿，相关四套 85 项通过，最后补充改目标断言后独立 7 项通过。详见手机 `docs/maintainers/REMOTE-FINAL-SCOPE-REVIEW-20261014.md`。
- 取消批准及上述切站修复之后，手机完整回归 **116 套件 / 1,920 项通过、8 项原有跳过，308.923s**。独立复核随后又复现：仅 `approval.resolved` 增量先于 HTTP ack 到达、快照时间未推进时，已完成问答的持久回执可能残留并挡住换 API。已最小修复：要求当前配对/会话、同问题 ID、phone/desktop 明确终态、非倒退时序，按原 UUID 结束回执；隐藏、本地过期、缺失、旧终态或新配对不清理。新增 15 个真实组件反例，先红后绿，9 套 324 项专项通过（93.491s）。该阶段冻结全量 **116 套 / 1,935 项通过、8 项原有跳过（219.226s）**，typecheck 通过。这是包含问答/切站/取消修复的历史快照；最新 1,958 项结果如下。
- Bridge agents 先前合并全量未通过：`TestCodexCloseBoundedWithOwnedNotifySinkBarrier` 触发关闭时限，后续多项测试的进程树清理累计导致整包超时。独立专项复现后已把 transport 与 worker 的 drain 改为共享五秒截止时间，保留原 12 秒断言；系统 terminator 改从可信系统目录解析，并在运行期间持有原进程句柄，避免并发 Wait 后数字 PID 被复用。最新专项 14 项通过（49.998s）。之前 full/race 279.553s / 473.889s 的通过快照不包含最后的句柄保护和协同 settings 补丁；待两处冻结后重新完成的整包结果见下文。不能笼统称 `taskkill` 单次无限等待，也不能沿用旧结果作为最终通过。

R02 跨任务来源导航、R03 原生任务管理、R04 计划/审查操作和 R08/R09 原生边界尚未全部实现，本增量未冒称补齐。发布前还需真实 Android、双站弱网、原生宿主矩阵与持续负载验收。本轮未发布。

### 最终冻结快照验收进度

- 手机此前 1,935 项快照之后又补发送能力动态撤回门禁与事件读取限流：最新冻结全量 **116 套 / 1,958 项通过、8 项原有 skip（219.031s）**；TypeScript 通过。新增 16 项真实组件门禁、7 项限流用例，相关五套 **303/303 通过（138.231s）**。全量前后选定的 338 个 TS/TSX/Kotlin/JSON 与 package 输入 hash 相同：`271ac4a42ea108797a2aad727369971eaeaeed40b9f7d2aa49bdf7992b89305d`；此后仅发布工程与文档。完整边界见手机 `docs/maintainers/REMOTE-FINAL-SCOPE-REVIEW-20261014.md`。
- 桌面 Companion 同源码再跑：105 项通过，0 skip（11.933s）。包括回环 HTTP 在原生派发前断开不能继续调用，以及派发后断开不停止已经开始的调用。
- 协同 `settings/context` 修复已冻结：本机显式应用的 API 使用独立本地 context；远程来源始终用授权快照，不能复用不同来源的忙碌本机 worker/审批；手机 JSON 不能伪造内部本机上下文。Codex/Claude 合成 worker 与 hubclient 专项 race 通过（6.866s / 1.665s）。
- Windows 最终 agents 普通整包通过（209.375s），最终同源码 race 全包也通过（258.341s），无 race 报告。每轮 322 顶层测试 / 687 含子例通过，2 项显式 opt-in 原生探针 skip，0 失败；agents+testfake 源码 hash 两轮前后不变。
- 协同 Bridge 最终 config/gateway/hubclient/console 全包 race 通过（2.780s / 9.415s / 68.387s / 94.259s），所涉包 vet 与 Bridge `build ./...` 通过。hubclient 首轮 `TestStatusAndPushDoNotDeadlock` 曾超过其 2 秒断言，无 race 报告；同源码独立三次专项通过（5.119s），再独立全包 race 通过（68.387s）。保留首次超时，未调整源码或断言，不能宣称高负载下永不超时。
- 协同对话后续的本机 continue 配置、账号额度重置 CAS 与成员/焦点保护已再次冻结：最新 hubclient race **56.226s**、codexaccounts/console race **20.909s / 92.204s**，对应 vet / Bridge 全仓 build 通过；六套 UI 脚本 **140/140**、最后成员样式专项 **16/16**、实际浏览器合成后端宽/窄验收通过。早期两秒断言超时记录仍保留；不把 UI 合成后端当作真实 OAuth 或官方额度消费验收。
- 旧适配审查子代理启动认证失败，没有形成审查结论；已由新只读代理接续，不把失败记成通过。CodeRabbit 限制仍见下文。

## 本轮修复明细

### 1. 备用中转站接收与切站

- `internal/hubclient/standby.go`：只在非 2xx 响应调用错误读取函数，成功响应保留给 JSON 解码。
- 限制响应体 32 KiB，拒绝截断、超大响应和尾随第二份 JSON。
- `standby_delivery_test.go` 覆盖真实回环 HTTP 轮询、B 站切换及回执；修正 `standby_test.go`，要求错误确实来自绑定检查，避免 EOF 导致假通过。

### 2. 撤销授权与原生桌面派发

- 新增 `internal/hubclient/native_admission.go`、`internal/desktopcompanion/remote_admission.go` 及测试。
- `client.go`、`phone_binding.go`、`dispatch.go`、`desktopcompanion/active.go`：登记绑定身份、取消旧身份的待发请求、在状态预检和 HTTP 派发前重新验证。
- 普通同身份重连不取消 pending admission；兼容旧 transport，但仍要求晚期 command/source guard。
- `../desktop-companion/src/gateway.mjs`：检查 HTTP 请求中止信号，覆盖 ledger.get、catalog、ledger.begin 之后的首次派发窗口；已派发的 native 调用仍使用租约信号，不与回环网络断开绑定。
- `../desktop-companion/tests/gateway.test.mjs`：三项竞态先失败再修复通过；另验证已派发调用不断、重试仅执行一次。
- 若持久 reservation 已写入而取消发生在 invoke 前，保留不确定回执，不删除记录来换取潜在重复执行。

### 3. Windows 项目目录与启动速度

- `allow.go` 与新增 `allow_path_windows.go`、`allow_path_other.go`：Windows 在文件访问前拒绝 UNC、device/network roots；逐层解析本地链接，阻止网络重定向。
- 目录归属同时使用真实祖先文件身份校验，拒绝大小写敏感目录中的同名兄弟越界。
- `allow_path_windows_test.go` 在临时目录中实际验证本地/网络符号链接以及 Windows case-sensitive 兄弟目录；本机均通过，没有以 skip 代替。
- `dispatch.go`、`allow_admission_test.go`：已保存项目先走本地授权，不先枚举所有 Agent 历史，减少本地启动的额外等待。
- 不宣称抵御拥有本地目录修改权限的攻击程序进行所有文件系统 TOCTOU 竞争；这里不是句柄级沙箱。

### 4. 图片资源预算与恢复

- `attachments.go`，新增 `attachment_budget.go`、`attachment_budget_test.go`。
- pending 最多 8 个上传、48 MiB base64；完成缓存最多 1,024 文件、256 MiB。保留单图 8 MiB、单轮四图的限制，修正 base64 padding 导致恰好 8 MiB 被误拒。
- 分片计数/字节量在同一锁中更新，重复分片不累加；失败、超时、完成释放 pending 预算。
- 扫盘每分钟执行，不为每个分片重扫或重写索引；容量不足再强制回收过期文件，定时清理随 Client context 停止。
- 清点目录失败时不沿用旧容量快照。Windows `ReadDir` 对普通文件返回类似“路径不存在”的情况有独立回归。
- 读取 bindings、文件导入和图片内容使用有限读取；拒绝过期引用并检查实际短读长度。
- 配额是单桌面进程内预算，不声称多个独立进程共享缓存时也具有跨进程事务配额。

### 5. 手机消息恢复与排序

手机工程：

- `src/remote/store.ts`：同 ID 的重放先恢复已确认的 `messageOrigin`、`parentId`，再匹配本地回执；旧普通消息不能吞掉后来相同文字的本地草稿。
- 内容新旧仍依据单调版本与 terminal 规则；显示位置另取最早有效 `seq/orderSeq`。旧 partial 可以修正位置，但不能把最终内容退回未完成状态。
- `remote-conversation-metadata.test.tsx`、`remote.test.ts` 覆盖 live/history 委派来源丢失、子任务来源丢失、同文回放、final/partial 交错和重排。

### 6. 手机通知生命周期与切站

手机工程：

- 新增 `src/remote/notification-preferences.ts`：只初始化一次，初始化完成前不启动后台副作用；用户的新设置不会被迟到的旧数据库读取覆盖；快速切换按序持久化。
- `notifications.ts`：统一读取开关，关闭时停止跟随；慢启动的 watch/standby 不得恢复已失效状态。
- `push.ts`：关闭时注销旧注册；token 获取和 Hub 注册完成后复核站点、电脑和配对凭证；同站重配对也更新注册。无推送能力的 Hub 正常结束，不无限重试。
- Android `SalcaraNotifyModule.kt`：用原生私有偏好保存布尔开关，未迁移前默认关闭；本地通知、FCM、watch 走统一带锁门禁。关闭时清理本模块的任务/待命通知，不清其他模块通知。
- `SalcaraPushService.kt`：JS 未运行时仍执行通知开关；系统收回通知权限时不因 SecurityException 崩溃。
- `SalcaraWatchService.kt`：旧 config 的迟到事件不弹通知；旧站 401 不停止新站的服务；批准按钮绑定随机 watch scope 和唯一 PendingIntent URI，并传入 sessionKey。旧 scope 不向新站发送决定。
- `remote-notification-lifecycle.test.ts`：11 项 JS 生命周期/竞态及 Android 源码契约检查。源码契约检查**不等于 Kotlin 编译或真机测试**。
- 本轮遵循 React best practices 的单次初始化和事件订阅管理建议，没有新增依赖、重做布局或更改动画。

### 7. 发布前追加：动态能力与限流退避

- 手机权威 Agent 目录加载后，已删除/不可用的 Agent 不再用旧 prop 复活发送能力；普通发送、排队、Continue 在实际点击/派发时重新读取当前 owner/capability。Claude Desktop Code 用自身描述，不借用 Claude Code 的许可。已接受的任务不被这类状态变化主动停止。
- 事件读取的 HTTP 429 保留原连接状态、错误与游标，继续通过既有指数退避恢复；不冒充离线，也不吞错形成 250ms 热循环。网络/401/403/503 等既有真实错误不会被下一次 429 抹去。
- 草稿、FIFO 和未知回执保留；UI 布局/动画没有改变。16 + 7 个新用例及最终全量见上文。

### 8. Hub 连接准入与真实容器

- 空长轮询与 App SSE 共享原有 per-owner/global 预算；即时事件页、reset 与 `wait=0` 不因等待名额满而被拒绝。
- App SSE 和 Bridge SSE 按**尚未退出的 handler**计数，不用“登记表里还在不在”判断名额。取消、撤绑、重配、删除/重建、切模式、阻塞 Write/Flush 都不能提前释放；原 handler defer 只释放原名额，全局计数改为 O(1)。
- Bridge 满额重连仅通知该设备旧连接退出，返回 429 与重试提示，不借用未释放额度，不关闭别人的连接；有余量时保留原替换行为。单设备仍可能与他人竞争全局预算，不能宣传独占资源保证。
- 三阶段反例均先红后绿；最终定向普通 **4.780s**、同源码 race 连续三轮 **24.813s**。冻结后 Docker Linux 全量普通 **12.801 / 7.860 / 29.928s**，race **32.905 / 18.382 / 293.242s**（hub/launcher/standalone），vet 和 amd64 镜像构建通过。
- 最终实际容器验收通过管理密钥登录/CSRF/会话撤销、重启保留配置、离线密钥恢复、nonroot/read-only/healthcheck 和更新状态；测试容器/卷已清除，没有真实数据。Windows 烟测改为验证源 TAR 的 0600 权限，不错误地拿 Windows 提取后的 mode 判定 Linux 权限；两处初始化/恢复都覆盖，10 项 helper 回归通过。
- 全部代码路径、红绿结果、镜像摘要、第一次烟测兼容性失败与最终结果记录在 Hub `docs/REMOTE-READ-ADMISSION-AUDIT-20261014.md`，不是目标服务器容量认证。

### 9. 桌面发布的源码、产物和标签绑定

独立复核确认原流程存在三个具体缺口：Linux 同提交测试仅靠说明要求，没有程序门禁；下载按 run/name 重新选择且只打印 artifact 摘要；`gh release create --target` 在已有同名标签时会沿用该标签，不保证它指向被测源码。

本轮新增 `desktop/scripts/verify-release-runs.cjs`、`download-release-artifact.cjs` 及两个测试文件，更新桌面 `.github/workflows/desktop-publish-verified.yml` 与 `docs/AUTO-UPDATE.md`：

1. 指定同一精确 SHA 的 Windows 构建 run 与 Linux source-check run；固定仓库、workflow 路径、manual event、完整成功状态和两个独立 run ID。查询失败、缺字段、错误来源/提交均停止，验证发生在下载之前。
2. 锁定 artifact ID 并再次验证 metadata 的 run/SHA/name/未过期/size/digest；不使用响应里的任意下载 URL。固定 API 下载字节经增量 SHA-256、精确长度和 ZIP 头检查，2 GiB / 5 分钟上限；独占新目录、背压写入，不全量读入内存。失败不输出 stderr/签名 URL/令牌，不覆盖旧证据或自动重试；通过后才解压，再执行既有六资产/签名/USTAR/便携包校验。
3. 产物通过后通过 GitHub 创建新 ref 原子预留版本标签，仅接受指向被测 commit 的确认；已有任何标签都拒绝复用，再以 `--verify-tag` 建未公开 draft。操作结果不确定时停止，不自动删除或重写。其他有权限的维护者仍需受仓库标签保护约束，本程序不宣称能阻止外部强推。

先红的 workflow 顺序用例证明旧流程缺 source-check 门禁；新增双 run / tag 70 项、固定 artifact 下载 50 项，联合 **120/120 通过（3.779s）**。最终整个桌面 Node 测试集 **201 项：200 通过、1 项原有 POSIX 专属 skip、0 失败（35.459s）**；YAML 解析、JS 语法和范围 diff-check 通过。测试用合成 API 响应、ZIP 和实际子进程，没有访问真实 GitHub 凭据或创建标签/release，没有启动真实 Electron。真实 CI 两个平台和 A/B 安装验收仍是独立发布门槛。

### 10. Hub 原件发布与手机发布说明

Hub 原先“测试后再次构建再发布”的路径不能证明上传的镜像就是测试过的镜像。现改为一次构建输出双架构 OCI 归档；平台、版本、非 root 配置、全部引用 blob 摘要、provenance/SBOM 及证明 subject 绑定验证通过后，从同一归档导入 amd64 做容器 smoke，并导出两架构二进制。发布 job 下载固定 artifact ID，再核对 build job 的归档哈希、镜像 index digest、源码 SHA 与 receipt，不重新构建。

- 同一规范版本跨 ref 串行发布；只有认证后明确的 `MANIFEST_UNKNOWN/NAME_UNKNOWN` 缺失响应才允许首次上传。已存在、鉴权失败、限流、网络异常均停止。复制保留全部平台及证明，上传后核对实际 index 字节；未知结果不自动覆盖重试。该检查不是注册表原子 CAS，仍须限制其他发布者的写权限。
- 实际 skopeo 1.22.3 / Docker 工具验证中发现新版 containerd 镜像存储的 `inspect.Id` 可能是 manifest digest。修复为从已捕获镜像 ID 的 `docker image save` 归档提取 config 原字节并核验摘要；不能只把 inspect ID 与 config digest 相等当作通过条件。
- 实际 BuildKit v0.32.2 的 scratch 文本构建发现：无镜像名称时 provenance subject 可能为空。构建阶段现设置目标 tag，但保持 `push: false`；空 subject 仍拒绝，不放松绑定规则。
- 真实工具回归已验证**合成**双架构 OCI index/证明往返与导入后的 config 原字节，以及真实 scratch provenance v1 的 subject 绑定。它不是正式 Hub 双架构/SBOM 构建、Linux CI 的 `docker-daemon` 导入、arm64 运行或 GHCR 上传验收。
- 手机构建前要求当前版本恰好一份非空、非链接的发布说明；兼容有/无 `v` 文件名，但缺失、多义、空白均停止，不再自动生成说明掩盖缺失。

代理冻结后，根任务独立复核代码并重新运行：Hub 管理/发布/OCI/私密归档四套 **59/59（3.314s）**；手机发布 workflow/清单 **12/12（4.678s）**，均无 skip。三份发布 workflow 的 YAML 唯一键解析、Hub 脚本语法、限定 diff-check 通过。上述测试不访问真实 registry、签名凭据或付费模型，没有执行发布。

文件范围、工具版本、首轮失败与清理限制见 Hub `docs/RELEASE-CONTRACT-AUDIT-20261014.md`。最终成功轮合成资源已清理；首轮仍保留一个合成镜像标签和临时目录，清理被执行策略拒绝后没有绕过，不能写成所有验证资源均已删除。它们没有真实凭据或用户数据，不进入发布产物。

## 多用户并发与容量口径

Hub 最初新增 `internal/hub/multi_user_release_test.go` 验证隔离；后续另修了读取预算生产内核，不能将本轮描述为“仅加测试”。隔离覆盖：

1. 32 组独立电脑/手机配对走真实 ServeHTTP，使用各自合成 secret、binding、phone hash 和 token。
2. 所有人故意使用相同请求 UUID，每人又并发重试两次：每人仅派发一次，回执身份正确。
3. 另一台电脑拿自己的有效凭证尝试代回当前命令，必须拒绝。
4. 相同批次 UUID、session key、event ID 不串用户；重放不重复插入，只能读到自己的正文。
5. 一人撤绑后另外 31 人仍可使用。
6. economy 下某一用户占满 8 条 pending，第九条得到限流，另一用户仍可派发并收回执。
7. 复跑既有同公网 IP 与 IPv6 /64 限流边界：他人的错误认证不能锁住已配对的手机或已登记电脑；限流表满不能封掉所有未知 IP。

上述并发及共享地址边界测试 `-race -count=3` 三轮通过，最后一次合计 4.693s。它证明这些合成场景的正确性，不是长时吞吐/网络容量结论。未配对的首次登记/扫码仍受 IP 防刷预算约束，同一网络批量安装可能需要等待；没有为了方便而移除防刷。

| Hub 模式 | 全局/每身份 pending 命令上限 | Bridge 活跃流上限 | App 活跃读取（SSE/空长轮询）全局/每身份上限 |
| --- | --- | --- | --- |
| economy | 64 / 8 | 64 | 32 / 4 |
| balanced | 256 / 32 | 256 | 128 / 8 |
| performance | 512 / 64 | 1,024 | 256 / 32 |

这些是并发准入限制，不是承诺可同时服务的付费用户数。实际容量受 CPU、带宽、图片、活跃任务量和事件频率影响；Go 软内存目标也不是 RSS 保证。不能为了宣传人数而取消每身份/全局预算。

## 验证记录

| 验证 | 实际结果 |
| --- | --- |
| Bridge 2026-10-13 全仓基线 `go test ./... -count=1 -timeout=600s` | 通过；后续冻结快照按上方增量记录验收，不能将基线当作包含所有后续修改。旧 e2e 在 Windows 有平台 skip，不能称原生真端到端全通过。 |
| Bridge 2026-10-13 `go vet ./...`、`go build ./...` 基线 | 通过；2026-10-14 最新范围见上方冻结快照记录。 |
| Bridge `-race` hubclient + desktopcompanion 早期联合快照 | 通过：45.771s / 28.653s；包含缓存失败清点修复，但不是最后的 hubclient 结果。后续本机 continue 配置修复的 hubclient 最终 race 为 56.226s，见上方冻结记录。 |
| Companion `npm test` | 105 tests 全通过，0 skip；包含新增的三处待发取消竞态。 |
| 手机 TypeScript | 通过。 |
| 手机 2026-10-13 基线 Jest | 105 suites / 1,740 tests 通过；8 suites / 8 tests skipped；213.848s。后续增量结果见上方 2026-10-14 记录。 |
| Hub 并发隔离及共享 IP 边界测试 | Windows 三轮 race 通过。 |
| Hub Docker Linux 最终活跃预算快照 | 全量 normal/vet/race 与 amd64 构建通过，包含 App/Bridge SSE 和空长轮询补丁；各包耗时、镜像摘要及限制见第 8 节与 Hub 专项记录。 |
| Docker 配置与真实本地容器 | 两份 compose 合成配置检查通过；最终镜像的实际隔离容器 smoke 通过管理认证、重启、撤销/离线恢复、nonroot/read-only/healthcheck 和更新状态，见第 8 节。不是生产服务器或 ARM 验收。 |
| 桌面发布与安装器 Node 最终测试 | 201 项：200 通过、1 原有 POSIX skip；不是实际 Electron A/B 安装验收。 |
| Hub / 手机发布工程最终独立回归 | Hub 59/59、手机 12/12；三份 YAML、Hub JS 语法和限定 diff-check 通过；具体工具证据边界见第 10 节。 |
| 格式 | 本轮 scoped `git diff --check` 与新增 Go 文件 gofmt 检查通过。 |
| Android Kotlin/APK | 本机没有 Android SDK/Gradle 构建工程；没有完成本轮原生编译、安装和 FCM 真机验收。 |

Hub 容器运行验收仅创建本地 audit 镜像/临时容器/临时数据卷。该烟测结束后临时容器及仅含合成密钥的数据卷已删除，审核镜像保留；没有触碰现有业务容器或数据卷，没有挂载 docker.sock。另一个 OCI 工具首次失败轮的合成标签/目录残留见第 10 节，不与本烟测的清理结果混写。

### 协同结果补记

同日已核对协同任务的 `QUOTA-MAINTENANCE-CLOSURE-20261013.md`：账号额度后台维护已完成专项验收，不再标为待实现。只刷新当前授权且实际用于调用、需要额度观察的账号，并发为 2；保持未知/过期额度拒绝准入，并处理凭据轮转、退避、关闭和迟到写回。单纯导入但未启用的账号不因此被轮询。

其独立证据：codexaccounts/console 两包普通测试、race、vet 和全仓 build 通过，codexruntime/gateway/apiservice 集成回归通过；账号卡 77 项 Node 测试和宽/窄浏览器交互验证通过。账号卡调整属于该协同任务，不归属本轮“不改布局动画”的远程修复范围。未调用真实上游、未发布。该专项结果不能替代 Android 原生验收或整个合并工作树的发布回归。

同日后续已核对 `MOBILE-F04-F07-CLOSURE-20261013.md`，手机 F04–F07 由协同任务完成并冻结编辑：

- 会话别名按配对凭据、电脑、会话隔离，统一显示、搜索和改名操作；没有授权证明的旧名字不自动迁入。
- 远程图片接入共用预览的保存/分享，支持 PNG/JPEG/WebP；独立分享副本、内容身份和预览交互代次阻止迟到结果污染新图片或新配对。
- 下载入口已接正式桌面 Releases；冷启动只回收具有 manifest 所有权且没有持久引用的远程图片。检测到消费者/发布者后停止回收，不删仍被草稿、队列或历史引用的文件。
- 共用图片预览的缩放、拖动和动画接续边界另行补修；这些手机源码由协同任务负责，本记录维护者没有并行覆盖。

测试版本边界：110 suites / 1,841 tests 通过、8 tests skipped 的全量结果发生在最后的预览代次与手势补修之前。之后 8 suites / 314 tests 专项通过；末尾两行手势起点修复后，14 项手势专项再次通过。最终 typecheck 和 scoped diff-check 通过。不能将 1,841 项全量结果标注为“最终补修代码已完整全量复跑”。生产组件加合成原生适配的宽/窄浏览器验证无应用错误或警告，但不证明 Android 相册落盘、分享接收、双指触屏事件或真机手感。原生构建和真机验收仍未完成，未发布。

## 安全扫描与审查口径

已封存标准 Codex Security 扫描 `cb3cd61d-7ad1-4069-b142-7841855fabe7`，范围是 Bridge `internal/hubclient` 的 77 个 Go 文件（32 生产、45 测试），不是所有工程的无限范围验证。

原快照报告列出：native 撤绑首次派发窗口（medium）、Windows UNC 访问顺序、大小写敏感目录边界、附件缺总体配额（后三项 low）。上述对应修复及回归在本记录中列出。原扫描报告保持封存，未篡改为“零问题”。

工具返回的扫描累计计数：input 18,136,095；其中 cached input 16,751,104；output 100,114；reasoning output 26,248；total 18,236,209 tokens，5 threads，usage coverage complete。这是扫描工具累计统计口径，不是调用用户的上游 API 消费。

CodeRabbit CLI 安装在当前 Windows shell 返回 `Unsupported operating system: mingw64_nt-10.0-26200`。没有 CodeRabbit 审查结果，不能写成“0 issues”；需在支持的环境安装、认证并运行。这里的测试与独立安全核查不是冒充 CodeRabbit 的结果。

## 不可隐瞒的剩余能力与发布门槛

- **原生 Codex 的完整远程控制仍未开放**：启用生产远程来源保护 (`RemoteExecutionArmed`) 后，当前无法证明原生宿主实际使用的 API/账号来源，故 `agent_status.go` 明确关闭 native send、approval、modelOverride、navigate，并移除旧账号归属。Companion 安装/授权/有效租约与 105 项合成回归并不能解除此限制，也不会自动改成 CLI。原生 stop、turn steer/运行中引导同样未开放；忙时本地队列不等于原生 steer。不能把托管 Codex CLI 能用宣传为原生 Codex App 完整投影控制已验收。
- Claude Code：按 API 路径适配。Claude Desktop 的 Code 工作流由 Bridge 托管 Claude Code executor，不是接管原生 Desktop 的同一 worker；Chat/Cowork 仍为只读历史。
- Claude teammate-message/SendMessage 跨任务来源专用映射仍不完整；已有子任务展示不能宣传为所有协作功能完全对齐。
- Hermes 只有扩展 adapter 合成验证，未完成生产接入。
- 远程图片保存/分享、会话别名一致性、正式下载入口和图片引用感知回收已由后续协同任务补齐，见上方 F04–F07 补记；已纳入最新手机 1,958 项合并回归，Android 真机验收仍缺，不能继续列为“未接线”，也不能写成“真机已通过”。
- 账号池 quota 新鲜度闭环已由协同任务完成专项验收，见上面的协同补记；发布仍需对合并工作树验收，不能将本轮旧全仓基线当作后续所有改动均已全量复跑。
- Android 真机必须覆盖：关闭通知后杀进程收到 FCM；快速开关通知；推送注册途中切站/重配对；旧站慢响应和旧批准通知；应用升级后的偏好迁移。
- 真机远程必须覆盖 Codex/Claude API 的实际版本矩阵、双站弱网/休眠唤醒、收发途中重连、带图恢复和重启回执。
- 多用户容量必须在计划采用的服务器配置上做长时压力与资源监测；本轮不宣称 32 人压力容量保证，更不宣称无限人数或零延迟。

**建议：以此为内核修复候选，不跳过 Android 原生构建和真机跨站验收直接正式发布。**

## 2026-10-14 新对话续接：Hub 入队边界与更新恢复

本节为后续增量，不把先前手机/桌面结果当作协作对话最新修改的全量验收。

Hub 新增两处修复：来源检查后到实际指令入队前重新核验取消、撤绑、连接身份、服务关闭和 standby TTL；Linux launcher 的卷锁由 Hub 子进程共同持有，启动器异常退出后等旧 Hub 真正退出才允许接管。后者原本另有 standalone 数据锁保护，不能写成已复现真实用户数据双写。八个发送边界场景、真实 SIGKILL 进程反例均已验证。

新增真实 TCP 慢消费者/FIN/RST/重新准入/游标重放证据，并使用实际 Hub 程序验证初始化、更新、重启、下载中断、健康失败回退及合成配对/项目/设置/管理凭据保持。两版实验程序为同源码不同版本标识，不是历史真实发布版的数据迁移认证。

冻结后本地 Dockerfile 内 Linux 全量 normal/vet/race、amd64 构建和实际容器 smoke 全通过；Hub 发布 Node 59/59；根任务独立复跑实际更新恢复 22.63s 通过。构建前后 97 文件 SHA 不变。具体耗时、镜像 digest、复跑方式、范围和剩余放行条件见 Hub `docs/HUB-CONTINUATION-20261014.md`、`HUB-TCP-RECOVERY-20261014.md`、`HUB-UPDATE-RECOVERY-20261014.md`。

本轮未修改手机/Bridge输入/账号/桌面启动UI；该范围归协作对话。原生来源门禁、Android真机、真实历史升级、正式签名/HTTPS、多架构/ARM和目标服务器容量门槛仍保留，未发布或部署。
