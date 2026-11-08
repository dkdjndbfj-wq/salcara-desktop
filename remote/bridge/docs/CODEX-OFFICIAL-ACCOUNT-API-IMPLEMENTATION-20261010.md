# Codex 官方账号与本机 API 模式实施交接（2026-10-10）

2026-10-11 追加复核：Token/JSON 批量导入、重复凭据拒绝、refresh-only 交换、loopback-only OAuth/手工回调、真实 usage/套餐、剩余订阅 hint、完整本机池策略、scoped 工具 Key、启动/恢复撤销保险、命名 Chat/WS、已知 backend-api 路径、客户端模型 namespace、专用 Images/队列/设置和持久 usage ledger 已补。官方 Runtime 的文本、流、图片及 refresh 均拒绝重定向；固定官方请求在每次执行及刷新后复查 quota reserve。用量缓存来源原子版本、未知费用与 UI 旧请求错误隔离已修复。当前矩阵见 [Cockpit 逐项复核](COCKPIT-PARITY-REVALIDATION-20261011.md)，最新整仓 ordinary/race 各 1035 主测试、9 skip、0 fail 及最终 UI/源码摘要见 [继续修复记录](PROXY-AGGREGATION-CONTINUATION-20261011.md)。本文下方历史验证结果保留，不替代本轮真实账号验收。

本文用于代码审查和部署前验收，不是发布公告。工作区已有其他开发改动，本轮没有重置、删除、提交、发布或部署它们。历史调查文档只作实施前基线，以本文列出的实际实现和限制为准。

## 范围和产品关系

本轮独立实现 Cockpit 风格的账号库、本机 API 服务和远程来源边界。参考 Cockpit 提交 `be24ce6171279741c2cd141bc02bd9f380f4b67c`（1.3.66）能力，不复制其 CC-BY-NC-SA 应用源码或界面。实际依赖为 MIT CLIProxyAPI/v7 `v7.2.157`，许可保留在 `THIRD-PARTY-NOTICES.md`。

用户允许修改 UI，并要求沿用原先设计风格。账号/API 新能力复用现有 Salcara 卡片、按钮、输入框、弹层和颜色间距；关闭/切换面板后的迟到结果不重新打开面板，保存中继续编辑保留新草稿。手机会话侧栏的实施记录见手机仓库 `docs/maintainers/MOBILE-CODEX-SIDEBAR-20261010.md`。

三种凭据相互独立：

| 凭据 | 保存及使用位置 | 不可替代的用途 |
| --- | --- | --- |
| 上游 API Key / 官方账号 token | 电脑账号库；官方凭据使用受保护专用存储 | 上游模型认证 |
| 命名本机客户端 Key | 创建或重置当次返回；配置仅保留随机 Key 的 SHA-256 摘要 | 本机 `/codex-api/v1` 认证 |
| 手机/电脑配对凭据 | 对应站点的配对存储 | Hub 设备连接；不能证明模型来源 |

## 实际实现

### 账号库及反代执行

- `internal/codexaccounts/`：严格导入 Codex `auth.json` token 对象、CLIProxyAPI Codex 记录、版本化批量包；文件由用户明确选择，不扫描或自动读取本机登录文件。
- JSON 拒绝重复、歧义字段及超限输入。预览和账号 DTO 不含 access/refresh/id token 或真实上游账号路由 ID。
- Windows 使用当前用户 DPAPI、私有 ACL、原子写入；其他系统目前是 0700/0600 文件权限保护，不宣传为加密存储。
- 刷新 singleflight；取消、删除、重新导入/登录以版本围栏阻止迟到结果复活账号。正常 token rotation 不改变 `IdentityEpoch`；显式身份改变会改变它。
- refresh 返回不同的显式 provider account ID 时进入 `needs_login`，不发布另一个账号的 token。轮换成功但保存失败保留私有 pending 结果，先 `RetrySave`，不再复用旧 refresh token。
- 浏览器登录独立实现 pinned MIT CLIProxyAPI 的 legacy Codex CLI code/PKCE 合同；callback 只监听 `tcp4 127.0.0.1:1455`，授权 URL 仍使用该合同的 `http://localhost:1455/auth/callback`。state、Host、method、path、重复 query、长度和单次消费严格校验；取消/超时立即关闭 listener，不再使用 SDK 绑定所有网卡且可能残留 waiter 的 callback server。授权页 URL只在 pending 状态展示，所有终止状态会清除。
- 本机跳转失败时可向当前 login session 提交完整 callback URL。后端不请求该 URL，只允许与该 session 实际 loopback redirect 完全相同的 scheme/host/port/path，并复用同一 state 和单次消费校验；裸 code、旧 session、错端口/路径、user/fragment 和重复参数均拒绝。替换账号仍需通过保存前 epoch/身份围栏。
- `internal/codexruntime/`：惰性启动 SDK 执行器，真实 readiness；bootstrap 服务无凭据且拒绝业务路由。执行固定受保护账号，SDK 不独立刷新、写凭据或挑选池成员。
- 同账号 401 只允许在尚未输出前刷新重试一次；部分流不重放。body 关闭取消生产者，避免后台泄漏。
- 当前是 `legacy_codex_cli`，后端为 `https://chatgpt.com/backend-api/codex`，不是新版 SIWC 动态注册登录。

### 官方账号状态、套餐与额度

- 用户可在官方账号面板显式调用 `GET https://chatgpt.com/backend-api/wham/usage`。Bridge 只使用 Salcara 受保护账号库中已明确导入的 token 和显式 provider account id；不会扫描或读取 Codex 原生 auth 文件，也不会自动轮询。
- 请求使用短超时、禁止重定向和固定 production origin/path；响应限制为 256 KiB，并沿用严格重复键、深度和字段预算。浏览器 DTO 只含归一化 `plan_type`、额度窗口、credits、spend control、来源和时间，不含 bearer、provider account id、响应正文或原始错误。
- `free/go/plus/pro/team/business/enterprise/edu` 家族由 provider response 的已知枚举归一化；安全但未知的短枚举保留为 `unknown`，不会猜成 Pro。导入/JWT 套餐继续标记为未验证 hint。usage schema 没有订阅到期字段；若导入字段或 JWT 的明确 `chatgpt_subscription_active_until` 声明存在，界面按 `import_hint`/`jwt_hint` 展示原到期时间、剩余时间、来源和观测时间并标记“未验证”，否则保持未知。token `expiresAt` 始终单独标成“凭据有效期”，refresh-only 内部过期哨兵不会出现在公共 DTO。
- observation 仅在内存保存并绑定 credential epoch 与 identity epoch。有效期为 5 分钟和所有已出现 provider reset 时间的较早值；Bridge 重启、账号替换、token rotation、删除、登录或跨 reset 都会使旧值不可用于路由。
- 官方池成员可配置 0–99 的 `quotaReservePercent`。0 保持原有行为；非零值要求显式刷新得到的 fresh observation，并综合主/次窗口、精确匹配本次模型的 `normal_model_slug` 附加窗口以及 `spend_control.reached`。未知、过期、达到阈值或 provider 明确拒绝均停止选择该成员；选择路径不发网络请求。
- 若 access token 已明确过期，额度读取先通过现有单一刷新所有者完成受保护存储的 rotation，再重新捕获身份/credential 围栏。usage GET 的 401 只允许在同一身份下刷新并重试一次；pending save、replacement 和 invalid_grant 均 fail-closed。

### 本机 API 模式

- `internal/apiservice/`、`internal/config/codex_api.go`、`internal/console/codex_api.go`：独立启停、命名客户端创建/编辑/重置/撤销、固定账号、模型别名和并发上限。
- 仅 loopback，拒绝浏览器跨站调用；仍受控制台 Host/DNS-rebinding 边界保护。未开放 LAN/公网。
- 路由为 `GET /codex-api/v1/models`、`POST /codex-api/v1/responses`、`GET /codex-api/v1/responses`（WebSocket 兼容）、`POST /codex-api/v1/responses/compact`、`POST /codex-api/v1/chat/completions`、`POST /codex-api/v1/images/generations` 和 `POST /codex-api/v1/images/edits`。Chat 原生转发保留多选参数，跨协议走有界验证后的 SDK 翻译并保留 refusal；不能可靠转换的参数明确拒绝。图片默认关闭，可设 enabled/images_only 和每凭据 1–16 并发；JSON/有界内存 multipart 的字段、图像引用和大小严格验证，官方模型使用 pinned SDK 实际直接执行的 image ID。
- `/codex-api` 的独立客户端 Key 摘要可以持久化，但服务启用状态还绑定当前 Bridge 的内存 `GatewayRuntimeKey`。启动或租约轮换会清空本进程绑定；持久化的 `enabled:true` 只表示上次选择，状态接口会返回 `running:false`/`requiresStart:true`，必须由 Salcara 控制台再次显式启用后才接受请求。这样重启、其他程序直接开启或 Hub 直连不会继承旧 Key 的服务授权；未 arm runtime 的库测试仍保留显式兼容行为。
- 普通 Responses 与 compact 分开处理。SDK 会删除普通请求的 `previous_response_id`/`conversation`，因此明确拒绝这些有状态续接参数，避免悄悄丢历史；调用方必须发送完整 input 历史。compact 使用原生路由，不伪造跨协议压缩。
- 客户端默认并发 16，可设 1–128；服务总并发 32，包含上传、目录读取和响应流。上传前取得额度；正文上限 64 MiB，读取限时 60 秒。限时仅限上传，不限制任务响应流时长。
- 慢正文超时先在 deadline 有效时关闭 body，并关闭连接，避免 net/http 响应结束阶段无限排空正文。
- 服务关闭、客户端撤销/轮换、别名变化、账号身份变化会拒绝尚未转发的请求；已转发请求继续排空，不切账号或中断任务。
- 所有需要本机 gateway 的工具连接（包括 reverse-proxy、OAuth/official 来源）都使用 Bridge 当前进程的内存 lease。启动时生成新 lease，服务端不再接受持久 `GatewayKey`；因此重启前的 Codex/Claude 配置和未由 Salcara 重新应用的外部进程会 fail-closed。无 runtime 的库测试保留显式兼容回退，但生产 `main` 总会先 arm lease。
- HTTP 统计只记录次数、状态、耗时和 inflight；新增独立有界 `usageledger` 持久记录真实 upstream token usage、任务状态、HTTP 和耗时，账号/客户端/route HMAC 匿名化。缺失 usage 为 null，不存 prompt/body/Key/URL/原始错误，也不编造价钱或将它当作账号 quota。failed 状态不被后续 success 复活；incomplete 即便有真实最终 usage 仍保持未完成。撤销且无活跃请求的 HTTP 统计条目淘汰。

### 模型目录

- `internal/hubclient/official_catalog.go` 提供复用的 `OfficialModelCatalog(ctx, account, refresh)`，接入远程 `models.list`、控制台刷新及本机 API `/models`。
- 官方账号普通读取可使用已存提示；显式刷新必须读取 Codex 原生 `model/list`，失败不返回旧目录假装成功。不发送 protected marker 到上游 `/models`，不做推理请求。
- 目录绑定账号引用、身份版本、来源策略和当前 worker 设置；旧读取不能覆盖重新导入、删除或更新后的账号。
- `nativeSource` 标注 native/saved picker hint；`catalogIsEntitlement=false`，能力信息保持 unknown。共享原生目录不证明该账号套餐、额度或模型权限。
- 官方显式模型遵守名称边界及允许/拒绝规则，不以共享选择器目录强行拒绝新模型；实际访问权限由上游检查。

### 反代界面内核

- `internal/console/official_accounts.go` 与 `web/codex-accounts.js`：浏览器登录、重新打开授权页、立即取消、手工回调 URL、文件/粘贴 Token/JSON 预览和批量导入、关联卡片、刷新、重试保存、替换导入、重新登录和移除。refresh-only 导入先保护保存再交换，预览完全不联网。
- 导入成功但卡片同步失败保留已安全保存凭据，可重试关联；删除先撤销受保护凭据，卡片写失败也不可继续执行。
- 已完成登录的迟到轮询须检查当前记录及身份版本，不能重建删除卡片或覆盖新身份。
- 严格本机请求解析拒绝大小写混淆、重复字段、尾随 JSON、null 等歧义。
- UI 关闭后清除秘密和计时器；导入预览、登录结果、创建/重置客户端 Key 的迟到结果不能填到另一个弹层。原文件作为 exact string 交给后端，不用 JSON 重序列化抹掉重复键。

## 远程来源与多站点切换

相关文件：Hub `source_policy.go`、`source_challenge.go`、`standby.go`、`command_receipts.go`；Bridge `source_guard.go`、`source_grant_client.go`、`standby.go`、`station_switch.go`；手机 `client.ts`、`store.ts`。

- Hub 可 open/restricted/disabled，策略变更使用新 revision；来源授权绑定完整指令哈希、请求 UUID、设备/手机绑定、策略版本、账号修订及有效期。
- 手机冻结指令和站点，遇 `source_grant_required` 时最多一次 challenge → grant → 重发原指令。站点切换 grant 使用同一 operation UUID，不生成另一写操作。
- Hub 内部 `source.grant.acquire` 不对普通 `/app/commands` 开放。challenge 120 秒、全局 256、每设备 32、保留指令总量 8 MiB；取得授权清除原始指令，完成回执后退役 challenge。durable receipt 仍可安全重放确认。
- Bridge 从保存的真实站点读取 challenge；凭据只直接 HTTPS 发给实际 API 同源签发器。Hub 自己配置一个外国签发地址，不能让电脑把其他服务商 Key 发过去。
- 凭据发出前和签发后检查手机解绑、当前连接、保存的站点 secret、API 修订；最终执行再校验真实来源，不能借 CLI 安装状态或可编辑标签获得权限。
- 受限备用连接只可取得“切换到这个保存站点”的内部授权，不允许任意 session.send、history、其他站点或其他设备指令进入备用通道。
- `sourceOnly:true` 支持只换服务器：来源 agent/accountId 用于认证，不改 RemoteAPI、卡片或模型。该标志进入 durable payload hash，真假互换不能复用回执。
- 手机从当前已选 Agent 的明确 API 派生 source-only intent，并存入 journal；重试沿用原 source。没有明确来源时不猜账号，受限模式明确拒绝。旧电脑端不声明 `station.source-only.v1` 时不发送该标志，避免旧版本误清模型。
- API+服务器同时切换仍走原原子切换路径；未确认消息、正在执行任务和未同步进度保持迁移门禁。
- 附件分块、桌面导航携带当前实际 agent/account handle，来源检查不会因为缺少内核元数据误拒绝。

真实签发器对接合同见 Hub `docs/SOURCE-ISSUER-CONTRACT.md`。仓库实现获取和校验框架，但没有编造 Sub2API 的真实验证端点。generic connector 当前只支持 `api_key + site_api`；受限模式的 official/oauth/reverse_proxy 与原生桌面实际进程证明仍需独立可信连接器，缺失时 fail-closed。open 模式是明确放开策略，不冒充可信来源验证。

### 2026-10-14 追加纠正

上一段保留 10 月 10 日历史结论。当前已存在保护官方账号 `station-official` 签发闭环，本轮补齐受管理 Claude Code 的官方 Responses 转换路径，以及唯一同源注册 issuer 的 `reverse_proxy + verified_proxy` 获取路径。OAuth 登录/导入实际关联为保护 `official_account`；原生 Desktop 实际来源证明、缺少真实 issuer 的第三方反代与新版 SIWC 不在已完成范围。详见 [远程账号连接器复核](REMOTE-ACCOUNT-CONNECTOR-CLOSURE-20261014.md)，不能再把受限官方账号整体列为尚未实现。

## 实际限制和部署前验收

1. 新版 SIWC 动态注册、JWKS/nonce/plan scopes 尚未实现。legacy 凭据不能标成新版登录，也不能发到不同 API 后端。
2. 生产浏览器登录已改为 Bridge 自有 loopback-only connector；旧 `SDKLoginConnector` 只保留兼容测试，不再由 `main` 选择。仍固定使用 legacy Codex CLI 的 1455 redirect 和 public client contract；端口被其他进程占用时会安全失败，不会改用未经授权的 client identity 或冒充 Codex Desktop hosted flow。
3. 不显示猜测额度；JWT 解码的名字/套餐/订阅到期只作提示，未验签，不作身份、权限或配额证明。只有用户显式触发且尚在 freshness 窗口内的 provider usage observation 能驱动 quota reserve；provider usage 未提供订阅到期时，只能显示明确导入/JWT hint，否则保持未知。
4. 不自动共享其他工具的可轮换 refresh token；导入后应避免多个软件并发刷新同一份 token。不同 token 所有者的跨进程仲裁尚无官方保证。
5. protected 官方账号及命名 API 已有本机 HTTP/SSE 适配的兼容 WebSocket，维护本连接完整上下文和 response id；裸及 /codex-api 挂载的 backend-api/codex models、responses、responses/compact 共用现有认证和模型围栏。不是原生上游 WS，其余任意 backend 路径拒绝。完整池策略与专用图片队列已有合成回归；图片池自动择成员仍见当前矩阵。audio 不在所指定 Cockpit handoff HTTP 基准清单中，不能将其状态与 Images 混合。
6. 本轮测试不读取真实 Key、auth.json、服务器凭据，不调用付费模型；没有真实账号 OAuth、Windows/Android 安装包真机、浏览器视觉或真实 Sub2API 签发器部署验收。
7. 不声称算法数学最优或已覆盖所有商业场景。当前目标是有界资源、来源固定、可重试回执和明确失败；发布前仍须完成以上实机与外部接口验收。

## 验证记录

以下结果均使用 synthetic fixture，不读取真实 Key、auth.json、服务器凭据，也不调用付费模型：

- Bridge：`go test ./... -count=1 -timeout=180s` 通过；`go vet ./...` 通过。
- Bridge race：`internal/codexaccounts`、`codexruntime`、`apiservice`、`gateway`、`config`、`console`、`hubclient` 七个包通过。
- 随后针对本地 gateway 进程 lease 的补丁又单独运行了 `go test ./internal/config ./internal/gateway ./internal/console -count=1`，三包通过；它验证重启/轮换后的旧 lease 不再被本地工具接受。
- 官方账号增量运行 `go test ./internal/codexaccounts ./internal/gateway ./internal/console -count=1`、对应三包 race 和 `go vet`；fixture 覆盖官方 usage schema、额度来源脱敏、过期令牌预刷新、401 单次重试、identity/epoch 围栏、provider reset freshness、spend control、模型独立额度、reserve 阈值、订阅 hint 隔离，以及 browser code/PKCE、loopback callback、取消/超时、token 响应边界/禁重定向、手工 callback 的 session/state/地址/单次消费围栏。没有读取真实账号或发起收费推理。
- Hub：`go test ./... -count=1 -timeout=180s` 与 `go vet ./...` 通过；Hub race 的 `internal/hub` 通过，`internal/standalone` 在 PBKDF2 race 检查中达到超时，不能标记为全量 race 通过。
- Mobile：`npm run typecheck` 通过；全量 Jest 为 74 个 suite 通过、8 个跳过，989 个测试通过、8 个跳过（含本轮手机 Codex 风格侧栏回归）；侧栏专项 `remote-thread-dialogue-new.test.tsx` 为 91 个测试通过。
- 未完成真实发行包、真实账号/签发器、Windows/Android 真机、浏览器视觉和外部 Hub 部署验收；这些不能用合成测试替代。
