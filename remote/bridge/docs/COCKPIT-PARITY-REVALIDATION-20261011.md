# Codex API / Cockpit 功能复核（2026-10-11）

## 2026-10-13 授权生命周期与导入兼容复核

- 浏览器授权保持实际绑定的 listener；1455 不可用时回退 1457，授权链接、手工回调与 Token exchange 使用同一 redirect。两端口均不可用时提示改用设备码授权；不关闭其他进程。
- 授权期限覆盖 listener 排队，显示真实父/子 context deadline。明确取消与过期分别显示；Token 响应读取期间的 deadline 也保留过期分类，迟到完整响应不能落盘。回调成功响应有界排空后再关连接，修复“账号已成功、浏览器却显示连接失败”的竞态。
- 登录完成后的列表刷新有会话/轮询版本围栏，不能关闭或取消新授权；失败自动重试。取消后清掉旧链接和设备码，再查真实终态，正确处理授权已先完成的情况。
- 补齐 Cockpit `custom_headers` 与 camelCase 别名，只提取 Authorization 与显式 ChatGPT-Account-Id；保留工作区、拒绝冲突，其他 header 不传入执行层。OAuth 中的空 API Key/Agent Identity 占位兼容，实际混合凭据与畸形标记拒绝，覆盖嵌套及平铺身份字段。
- 第二轮核对兼容 Sub2API 同一到期时间的 Unix 数字/ISO 双重表示及 `personal_access_token` 模式。时间仅在 `expires_at` 按精确瞬间等价；PAT 必须 access-only，其他身份/凭据冲突检查保留。
- 关闭授权窗口及迟到 start 响应也后台核对真实终态，经过身份检查后关联已保存账号；重试有上限，不影响新窗口。导入已保存但 API/密钥库列表读取失败、畸形或过时均给出可重试状态，刷新不重新提交凭据，旧结果不覆盖新预览。

本轮验证与真实账号边界见 [授权与导入收尾记录](OAUTH-IMPORT-CLOSURE-20261013.md)。下方 10 月 12 日测试统计保留为历史记录。

## 2026-10-12 授权与导入补齐

本节更新下方历史基线的账号入口状态。继续以 `be24ce6171279741c2cd141bc02bd9f380f4b67c` 的实际 OAuth / import 源码核对行为，独立实现。

- 正式启动同时注册浏览器 PKCE 和设备码授权；控制台明确选择 `mode=browser|device`，提供打开/复制授权链接、设备码展示/复制、取消与浏览器完整回调粘贴。设备会话拒绝浏览器回调，结束后清挑战。首次状态查询失败仍可取消，临时断线继续查询，失效会话允许重新授权。
- 导入支持 auth.json、CLIProxyAPI、Cockpit managed 导出、Sub2API OpenAI OAuth accounts 包装、对象/数组、JSONL/NDJSON、UTF-8 BOM、Session 对象/转义 JSON、常用 Token/Header/账号/套餐/订阅别名。明确选择 Access/Refresh Token 时支持多行 TXT 与 JSON 字符串/数组。
- 文件可多选；客户端传原始文本，不先解析重写。整批共用 1 MiB / 64 账号边界，任一无效文件拒绝整批；保留重复键、别名冲突、跨容器拼接与重复凭据检查。预览无网络、无保存；确认后先保护落盘，refresh-only 记录再通过已有刷新所有者更新。
- 网页 Session 标记 `chatgpt_web_session`，仅查额度；禁止 SDK 导出、API 执行、Codex client 刷新及官方重置次数消费。替换时清除同凭据所有本地别名的 API 成员引用并停用失去主账号的客户端；重新 OAuth 可恢复 API 资格。
- API Key、Agent Identity 和只有密码的待授权记录属于其他账号类型，不由此 OAuth 解析器冒充可用凭据；混合其他平台记录也不会被静默丢弃。具体契约见 `internal/codexaccounts/README.md`。

新增回归覆盖两模式生产装配、真实 loopback synthetic 授权、回调隔离、跨模式替换/取消迟到结果、持久化重启、导入文件/格式切换竞态、查询专用 Session 的执行及消费边界。普通/race/vet 与 Bridge 编译通过；控制台相关 Node 交互回归 44 项通过。使用合成凭据与本地 provider fixture，未进行真实账号授权或发布。整仓测试曾因 agents 历史目录测试包累计超过 240 秒而超时，其他包通过；延长为 600 秒后，该包只报 Windows 生命周期 fixture 的 child-pid 空文件竞态。已将测试 helper 改为临时文件写完后 rename 发布，失败用例连续三次通过（25.847 秒）；生产进程逻辑未修改。这里不将修复后的专项复跑表述为最终整仓全部通过。

对照用户指定的 [Cockpit 服务交接说明](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/docs/CODEX_API_SERVICE_HANDOFF.md) 和同一提交的实际源码。固定参考已在本机只读 checkout 核实，未将 Cockpit 应用代码复制进 Salcara；Salcara 独立实现宿主和策略，MIT CLIProxyAPI/v7 `v7.2.157` 的许可见 `THIRD-PARTY-NOTICES.md`。

历史审计是实施前基线。下表对应当前代码，合成测试与真实上游验收分别记录，不能用 JWT、模型目录或进程存在代替真实权限证明。

## 账户与授权

| Cockpit 基准能力 | Salcara 当前实现 | 验收边界 |
| --- | --- | --- |
| OAuth 浏览器登录 | `codexaccounts/browser.go` 独立实现 pinned MIT SDK 的 legacy Codex CLI authorization-code/PKCE S256；监听 `tcp4 127.0.0.1:1455`，不可用时回退 1457；固定 auth/token/client，实际绑定 redirect 贯穿全程；随机 state、单次消费、严格 Host/来源/参数边界、有界 token exchange、禁重定向 | 代码及 synthetic provider 流程完成；真实账号未验收。未冒充 Codex Desktop 身份，未实现新版 SIWC。生产账号 vault 另持有 OS 独占 lease，第二 Bridge 返回 busy，不会并发刷新同一 vault |
| 取消、超时、重开授权页 | callback listener 跟随取消关闭并 join，成功响应先有界排空；面板显示真实期限与取消；结束后清空 challenge；排队也计入超时，父/子 deadline 均显示过期，旧弹层有围栏 | 已实现；两个回调端口均不可用时提示设备授权，不关闭未知进程 |
| 手工回调 URL | Service loginID 绑定活跃 connector flow；粘贴完整 URL，精确匹配该会话 loopback redirect/state，复用单次消费；只解析 URL，不请求它 | 已实现；不接受裸 code，不复用旧/其他会话回调；输入提交前清空 |
| Token / JSON / 文件 / 批量导入 | `token_import.go`、`import.go` 和控制台面板：明确选择 access token 或 refresh token、逐行最多 64 个；auth.json、CLIProxyAPI、账号数组及版本化包；camelCase alias 与 credentials/tokens/metadata wrapper | 已实现；拒绝重复键、别名歧义、超限、API Key 混入；JSON 原始字节不重序列化。预览不存储、不联网 |
| 仅 refresh token 的记录 | 先保护保存，再经唯一 refresher 换取 access token；最多 4 个并发交换；失败记录可重试，不能显示 ready | 已实现；内部过期哨兵不暴露为订阅或凭据有效期；第一次 exchange 可补首次 provider account id，后续身份改变拒绝 |
| 重复凭据 | access/refresh token 的私有摘要集合拒绝同批次、既有账号和 pending rotation 的重复；显式替换只排除原 owner | 已实现原子拒绝；不凭 email/JWT account id 自动合并身份，不暴露摘要 |
| 刷新与保护存储 | singleflight、身份/credential epoch、rotation 原子保存、DPAPI/私有 ACL、SDK 只读 snapshot、取消/删除/替换围栏 | 已实现进程内刷新所有者；其他软件共享同一 refresh token 的跨进程仲裁尚无保证 |
| 真实套餐和额度 | 显式 GET 固定 OpenAI `/backend-api/wham/usage`；主/次窗口、独立模型额度、code review、credits、spend control、reset 和观测来源；已知 free/go/plus/pro/team/business/enterprise/edu 分类 | 已实现 provider response 解析与刷新；真实账号响应未验收。未知套餐保持 unknown，JWT plan 保持未验证 |
| 剩余订阅时间 | `subscription.go` 读取明确导入 expiry 字段或 ID/access JWT 的 `chatgpt_subscription_active_until`；展示原到期、剩余时间、来源、观测时间，和 token expiry 分开 | 已实现展示和显式 expiry 策略的排序 hint；未验签提示不建立身份/套餐权限，不豁免刷新或额度检查。无明确字段时为未知 |
| 失效 token | invalid_grant/身份改变持久清除 refresh token，needs_login；写盘失败本进程立即禁止再次刷新、save_failed/RetrySave | 已实现；清理写盘失败后强制终止，不能保证未保存结果跨重启持久化 |

订阅到期的 Cockpit 源码参照是 `src-tauri/src/modules/codex_account_index.rs:36` 和 `codex_account_import.rs` 的明确字段；先前仅检查 usage schema 后将到期功能列作未实现的结论已纠正。到期 hint 的显示能力与真实 provider 认证分开。

## 反代、账号池与服务

| Cockpit 基准能力 | Salcara 当前实现 | 验收边界 |
| --- | --- | --- |
| 本机账号池 | 空策略保留原 priority/weight + 最小负载；显式支持 auto、random、single-account、额度高/低、套餐高/低、订阅提示早到期和 custom priority/weight/backup；模型、并发、冷却及 quota reserve 先筛选，会话继续固定 | 已实现内存调度；策略来自 Salcara 明确应用的源账号，single-account 和目录都固定该账号。套餐/额度仅用当前身份且仍新鲜的 provider observation，未知排后；订阅到期仅为未验证排序提示。远程/命名客户端不继承本机池 |
| 会话固定、并发、健康/冷却 | 命名空间隔离、4096 有界粘性、同凭据跨卡片/池/固定路由共享并发、退避/Retry-After 上限、指纹隔离迟到结果 | 已实现；部分输出不重放、不换账号。远程/命名客户端固定所选账号，不自动继承本机池 |
| quota reserve | 0–99 阈值；仅 fresh 当前 identity/credential observation；主/次与精确模型窗口、spend control 共同检查。固定官方请求也在实际执行及 401 轮换后逐次校验父模型和 image tool 模型 | 已实现；未知、过期、刷新失败或阈值不足 fail-closed，选账号不联网；不能用刷新前缓存授权轮换后重试 |
| 命名客户端 Key | 独立创建/轮换/撤销/停用/恢复、仅摘要落盘、固定账号、客户端与总并发 | 已实现；区别于上游 Key、工具连接 Key 和手机配对身份。可明确配置独立文字 `accountIds` 池与 `poolStrategy`/`poolAccountId`，空列表保持固定主账号；图片 `imageAccountIds` 单独调度，成员变更更新授权 epoch |
| 模型权限、别名与命名空间 | exact allow/deny 加 literal allow/deny prefix，deny 优先；可选客户端 `team/` namespace，精确剥离一次再解析 alias；目录、Key 本机检查、HTTP/WS 和 image tool 使用同一规则 | 已实现；命名空间和 ACL 前缀均不解释 glob，不能用 alias 或工具模型扩权 |
| 服务启停和重启保险 | 独立 enabled 意图 + 当前进程 RuntimeLease；running/requiresStart；每次启动重新显式启用 | 已实现；持久 Key/旧 profile 不能获得新进程权限 |
| 就绪和配置探测 | executor 随机 bootstrap，以及控制台 cookie 保护的本机 readiness / 客户端 Key / 模型规则检查 | 已实现本机验证；`upstreamVerified=false`。不伪装真实推理或套餐权限验收 |
| Models / Responses / compact / Chat | 命名 API 提供四类路由；Chat 原生多选与 SDK 跨协议工具历史、流/非流；native compact 单独路由 | 已实现受支持子集；会被 SDK 静默删除或改意图的参数明确拒绝，完整历史必须由调用者提供 |
| 官方及命名 API WS | protected executor + HTTP/SSE 适配的 loopback WS；命名 Key、无 Origin、连接授权和逐 turn 模型围栏；完整 input/output/加密 reasoning、本连接 response id、warmup；失败 turn 清 continuation | 已实现 Responses 兼容 WS；下游 gateway 保留会话历史与 response id，原生上游 turn 使用固定 endpoint、私有无代理拨号、无重定向、同账号首事件前刷新和关闭取消围栏；不转发任意 backend 路径 |
| 账号卡片操作 | 账号卡片显示官方套餐、额度、订阅提示、API 服务成员状态和官方重置次数；可直接加入/移出非主 API 服务成员，主账号通过“管理 API 服务”更换；删除账号会清理客户端池引用并停用失去主账号的客户端 | 已实现 CAS 成员更新、删除清理和旧响应 UI 围栏；“清除本地额度缓存”只清本地 usage/冷却；单独的“消耗官方重置次数”经二次确认调用受保护的官方消费流程 |
| images / image queues | `/v1/images/generations`、`edits`，JSON/有界内存 multipart、受保护官方同账号执行或固定 relay；disabled/enabled/images_only；每凭据 1–16 默认 1 的有界 FIFO 队列，Responses 显式 image tool 共用队列和 ACL | 已实现合成链路；`imageAccountIds` 是显式图片成员集，按 credential 去重后选当前任务最少者，生成/编辑/Responses 图片工具/下游 WS 图片帧共用队列、模型和 quota ACL；不自动扩展本机池或注入未请求工具。仅真实 image result 才能完成，空结果/部分流/迟到错误不会变成成功。真实官方 images 未验收 |
| 工具应用、备份、恢复 | 生产所有 relay/official 连接均走 scoped loopback gateway；源/工具/apply nonce HMAC；授权先保存再 Start；失败即刻撤销。恢复全检查后首写前撤销；失败回滚不复活 Key；旧 cleanup 不删新 apply | 已实现；默认直接写 URL/Key、旧 `/api/toolcfg` 一键配置或独立启动的旧旁路在生产返回 409，并在状态接口标出 `requiresWorkbench`；外部修改拒绝覆盖。服务关闭不自动替用户恢复工具 profile |
| 持久请求账本 | `usageledger` 记录真实 upstream usage、状态、HTTP、耗时和匿名账号/客户端/route；默认 1000 条/30 天，可配最大 4096/365 天，队列 128、原子保存、清理/重试、丢弃和读写错误可见 | 已实现 SQLite durable ledger（保留并迁移旧 JSON，Windows journal 与 ACL 隔离）；控制台支持时间/账号/客户端/route/model/state/status/tier 筛选、revision 分页、摘要分组、趋势、SQLite 错误恢复。价表有版本/并发更新/重算；上游实际费用与用户估值分列，未知用量或费率保持未知，创建缓存计价口径必须明确 |
| 用量总览与错误健康 | Store 锁内发布来源 revision，来源 Key/显示元数据指纹与请求序列围栏阻止迟到覆盖；每 Server 独立缓存。缺费用为未知，部分费用明确标注；模型容量与本机准入失败不冷却整份凭据，真实 429 仍冷却 | 已实现合成回归；官方/OAuth/非空 CredentialRef 不发普通站点费用请求。relay 用量响应禁止重定向，检查大小、读取错误和 null |
| LAN / WSL / 多端口 | loopback listener，仅本机连接 | 已实现专用 API 多端口，loopback/LAN 私有 IPv4 主机、当前进程 lease、命名 Key、Host/Origin/RemoteAddr 围栏；控制台仍只本机。WSL 镜像 localhost/NAT 主机地址计划已显示，未自动安装 relay、防火墙或修改 WSL profile |
| 远程来源证明 | source challenge/grant、指令哈希、策略/身份/账号修订、最终执行校验 | api_key+site_api framework 已实现；受限 official/oauth/reverse_proxy 可信连接器、原生实际 provider 证明仍缺，缺证明时拒绝 |

当前 Key 的持有者仍拥有该 Key 的权限。scoped key、apply nonce、epoch 和租约解决跨来源、跨工具、撤销恢复及重启复用；它们不证明每个原生进程一定由 Salcara 启动，也不能阻止拿到当前有效 Key 的软件使用它。

## 数据源与验证

- OpenAI Codex primary source：[`rate_limit_resets.rs`](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs) 的 `/wham/usage`，[`client.rs`](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client.rs) 的 Bearer/ChatGPT-Account-Id，[`types.rs`](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/types.rs) 的 schema，[`auth.rs`](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/auth.rs) 的已知套餐。
- Cockpit 同提交的 `codex_quota.rs`、`codex_account_index.rs`、`codex_account_import.rs`、`codex_oauth.rs`、`codex_local_access_routing_pricing.rs` 和服务交接说明用于行为核对，未复制其应用代码。MIT CLIProxyAPI 的 legacy client identity 和执行/协议能力保持 pinned 版本。
- 当前功能增量普通/race/vet 和 UI 回归均使用 synthetic fixture，不读取真实 auth.json/Key，不发起真实 OAuth、usage 或付费推理。最终稳定源码的整仓结果以 [继续修复记录](PROXY-AGGREGATION-CONTINUATION-20261011.md) 为准。
- 10 月 9 日、10 月 10 日及本日旧 1038 文件/717 主测试全量记录为历史基线，不能用于此次新增 OAuth/import/Chat/ledger/lease 功能的全量证明。

完整 Cockpit 基准对齐尚有上表明确的纯代码余项和真实链路验收项；不得将“已核对参考源码”写成“所有功能完成”。

Cockpit 的 `rate-limit-reset-credits/consume` 会消耗官方账号的不可逆重置次数。Bridge 现在提供独立的 prepare/status/consume 流程：消费句柄绑定 credential identity epoch，使用 UUID `redeem_request_id`，持久记录 prepared/consuming/succeeded/failed/unknown 状态，单账号并发合并，401 只用同一 UUID 刷新重试一次，网络未知结果禁止盲重放；官方端点固定且拒绝 API Key 账号。UI 二次确认后的“消耗官方重置次数”和“清除本地额度缓存”是两个不同动作。测试使用可注入的 synthetic transport；真实账号仍需用户在目标环境中验收，成功后应刷新 `/wham/usage` 确认余额变化。

服务 profile takeover 仍保持明确差异：当前 Codex API 启停只管理 Salcara 自己的 listener、租约和账号 Key，不会暗中修改或恢复 Codex 全局 `auth.json`/`config.toml`。已有 Workbench scoped apply 使用独立授权和 applied hash，不能被服务停用流程误当成服务拥有的备份。若以后加入 Cockpit 式 profile 接管，必须新增默认关闭的 service-owned manifest、目标文件备份、应用后 hash、外部修改检测和 restore nonce；在此之前不自动恢复任何外部工具配置。

图片接口只接受 pinned SDK 实际直接执行的 `gpt-image-1.5`、`gpt-image-2`、`gpt-image-2.5`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst`，不将任意图片模型改写成文本模型。外部 image URL 不由 Bridge 额外抓取，本地路径不读取，data URL/multipart 的格式、大小、数量和字段严格检查。官方执行的 HTTP/SSE/images 与 token refresh 均通过 Runtime 私有无代理 transport 拒绝 3xx；未修改 SDK/module cache 或全局 HTTP defaults。

## 2026-10-14 远程账号连接器纠正

上表“受限 official/oauth/reverse_proxy 可信连接器仍缺”是历史状态，不能继续作为当前代码结论。保护官方账号已有 `station-official` challenge/签发/执行闭环，本轮补齐受管理 Claude Code 路由及同源注册 issuer 的 `reverse_proxy + verified_proxy` 获取路径；原生 Desktop 实际 provider 证明仍缺。OAuth 导入经实际保护 vault 关联为 `official_account`，裸 `oauth` 标签不受信任。当前范围、证据和真实站点 issuer/实机边界见 [远程账号连接器复核](REMOTE-ACCOUNT-CONNECTOR-CLOSURE-20261014.md)。此追加不改变上方历史测试日期或结果。

## 2026-10-15 服务配置接管补齐

上方“服务 profile takeover 尚未实现”和“服务关闭不恢复工具 profile”保留为历史基线。本批新增服务专属 manifest、原始文件备份、跨进程锁、事务 journal、generation/revision 校验、独立连接授权和明确的应用/恢复入口；停止服务或正常退出恢复其拥有的配置，外部改动则拒绝覆盖，崩溃记录供后续显式恢复。Workbench 备份与服务备份互斥，普通启动服务仍不会暗中修改全局配置。

关闭流程还增加不可逆请求准入标记，防止并发网络或图片设置在退出期间重新放行 API。实现、最终专项普通/race、UI 证据和未验收边界见 [Codex API 服务配置接管](CODEX-SERVICE-PROFILE-20261015.md)。此增量不声称任意原生桌面进程的实际 provider 来源证明已完成，也不将旧构建视为包含新代码的验收包。
