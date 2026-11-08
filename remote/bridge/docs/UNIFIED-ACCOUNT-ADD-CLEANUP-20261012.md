# 统一添加与失效账号清理 · 2026-10-12

## 用户要求与界面

API 密钥库右上只保留一个“添加”入口，旁边新增“清理失效账号”；原有“API 模式”改为列表内的“本地 API 服务”卡片，管理入口仍打开原有配置。列表尾部重复的添加卡片移除。

“添加”弹窗分为 API Key / Codex 账号。API Key 复用现有地址、密钥与模型编辑器；Codex 账号内包含授权登录、Token / JSON、文件导入。授权登录仍支持浏览器及设备码。不是另开账号管理页，不复制 Cockpit 个人数据，保留 Salcara 样式。

授权正在进行时不能静默切换类型，取消仍可用。替换已有账号不提供转换成 API Key 的入口。切换到 API 编辑器会清除旧授权弹窗的临时状态；迟到响应不回写新弹窗。

## 清理合同

- 预览只读本机安全账号状态，不进行付费请求，不通过模型探测猜测失效。
- 只纳入确实为 `needs_login`、Access Token 已知过期或已不存在，且没有正在登录、刷新、额度查询、额度重置或待保存旋转凭据的 Codex 账号。这里“失效”指当前凭据需要重新登录，不声称账号永久不可恢复。
- 缺少 Refresh Token 也可能产生 `needs_login`，但不能据此认定 Access Token 已失效。只要 Access Token 非空且有效期未知或仍在未来，就保留；预览和提交都按服务当前时钟重新核验。预览后修改有效期或时钟回拨也不会沿用旧结论删除。
- `ready`、待刷新 token、刷新失败、网络错误、额度不足及普通 API Key 都不作为清理候选。
- 用户确认名单后提交固定 `id / epoch / identityEpoch`。服务端在凭据锁内再次检查状态与版本；已恢复、被替换或状态变化的账号保留。
- 凭据批量写入成功后才发布删除状态，随后清理精确身份匹配的 API 卡片和引用。新导入身份不会被旧确认误删。不会删除 Codex 聊天、项目文件或其他应用的原始账号数据。
- 凭据删除不能撤销。安全回执只用于恢复列表同步，不保存被删除的凭据；列表同步失败可重试同一确认快照。
- 清理按钮重复点击不会重复派发；关闭弹窗后的迟到结果不覆盖新界面。网络结果未知时不自动重发删除操作，要求重新检查。

## 代码与验证

前端：`internal/console/web/app.js`、`codex-accounts.js`、`shell.css`；测试：`scripts/test-codex-account-ui.mjs`。

后端：`internal/codexaccounts/cleanup.go`、`store.go`、`internal/console/official_cleanup.go`，由 `official_accounts.go` 注册路由。回归文件：`internal/codexaccounts/cleanup_test.go`、`internal/console/official_cleanup_test.go`。

所有测试均为本机合成数据，未操作真实凭据，未发布、提交或部署。

### 后端协议与持久化

- `POST /api/local/codex/cleanup/preview` 返回安全候选 `{accounts:[{id,name,epoch,identityEpoch}]}`。它不发网络探测、不改变账号状态，也不会把已经删除的回执当成候选。
- `POST /api/local/codex/cleanup` 只接受 `{accounts:[{id,epoch,identityEpoch}]}`，每批最多 64 项；拒绝重复 ID、缺失/多余/大小写混淆字段、重复 JSON 字段和不合法版本。返回 `{ok:true,deleted,skipped,syncFailed}`。`skipped` 原因包括 `changed`、`not_eligible`、`not_found`，均不删除账号。
- 同一服务锁内检查真实凭据状态、版本和待完成工作，再一次性原子保存整个 vault。写入失败不发布本批删除；成功后才撤销对应运行中引用。未决额度重置（`consuming` / `unknown`）同样不参与清理。
- 卡片同步仅移除 `sourceType=official_account`、`providerId=openai` 且 `CredentialRef + CredentialEpoch` 精确匹配旧身份的配置。普通 API、后来重新导入的身份、聊天、项目和文件不受影响。
- vault 同步保存最多 64 条最近清理回执，只含本地 ID / 版本 / 时间，不含 token，也不保存已删除凭据。系统时钟回拨不使新批次回执被旧记录挤掉。重启后仍可重放同一已确认请求并完成卡片同步；重放时 `deleted[].name` 可为空。
- 回执仅适用于账号仍不存在、墓碑版本仍为删除后版本、请求精确匹配旧版本的情况。相同 ID 后来存在新账号或版本变化时，旧回执不能继续清理。超过回执保留容量的历史请求会安全跳过，不扩大删除范围。
- `syncFailed=true` 表示凭据已删除、列表同步未保存，不代表凭据仍可用；界面保留本次确认快照提供“重试同步”。该回执不是恢复凭据的备份。

### 后端回归覆盖

15 个测试函数（含表驱动子用例）覆盖：只清理 `needs_login`、有效期未知/未来的 Access Token 保留、实际缺少 Refresh Token 导致的 `needs_login`、提交时重新核验有效期和时钟、所有待完成工作、重新导入/状态恢复/版本变化、vault 保存失败原子性、持久回执重载、回执不影响同 ID 新身份、容量及回拨、并发刷新、输入边界、配置精确身份匹配、配置保存失败后同快照恢复同步，以及安全 DTO 不含凭据。

已完成：两包全量 `go test ./internal/codexaccounts ./internal/console -count=1 -timeout=600s`（10.644s / 60.150s）、两包全量 `-race`（16.001s / 64.204s）和 `go vet`。最后回拨边界补充后，`go test ./internal/codexaccounts -run TestCleanup -count=1 -timeout=180s` 通过（1.299s），两包 `-race -run 'TestCleanup|TestOfficialCleanup'` 再次通过（2.310s / 2.822s）。这里全量任务可能启动于最后窄补丁之前，因此单独列出最后补丁定向结果，不混称全仓/真机验收。

进一步收紧“Access Token 仍可能有效”的保护后，两包 `-run 'TestCleanup|TestOfficialCleanup'` 再次通过（1.465s / 1.153s），相同范围 `-race` 通过（2.795s / 2.886s），最后两包 `go vet` 与局部 diff / 新文件尾空格检查通过。此轮未改变其他账号状态语义，也没有执行真实账号清理。

另按用户要求去掉手机对话标题下和新任务输入区重复的项目名（预览中的 Studio）。中央项目选择器、侧栏与真实 cwd 传输保留。

此前 UI/异步验证 66 条通过，两个 JavaScript 语法检查及局部 diff check 通过。独立复核另发现并修正两项列表竞态：缓存页面的初始慢列表不能覆盖清理后的新列表；过期的旧变更也不能递增读取代次，导致更新的变更结果被无效化。两项均有回归测试。后续本地 API 卡片与导入筛选的验证见下方增补。

截图 D01/D03–D09/D23/D24 等已使用当前真实组件、合成数据重拍并逐张检查；最终桌面 27 张、手机 42 张、Hub 13 张全部收入离线图册。没有执行任何用户账号清理。

## 普通 API 卡片补充

按最新卡片参考调整 `app.js` 的 `localAccountCard`，其他渲染函数由主任务负责。普通 API 使用与官方账号一致的轻色卡片：名称 / API 徽章、供应商 host、协议、脱敏 Key、基础 URL、已保存模型数、工具配置和 API 服务入口。编辑、读取模型、移除直接显示，不折叠“更多”。样式仅新增 `api-provider-*` 专属规则，窄卡片按钮可自动换行。

只读 `keyMasked`，不使用 `key` 字段或新增明文端点；地址展示去掉 userinfo / query / fragment。不编造 OAuth 绑定、余额、每日统计、运行状态。工具按真实 selected / applied / pending 字段区分“已应用”“待应用”“上次应用”，不把选择 API 当成启动工具；API 服务按去重后的真实客户端字段展示，“启用客户端成员”不是服务正在运行的证明。按钮统一打开既有 API 服务配置，不做未经确认的成员变更。

独立测试脚本 `node --test scripts/test-api-provider-card-ui.mjs`：9 条通过，覆盖直接操作、脱敏 / XSS、真实客户端角色 / 停用 / 未知状态、客户端去重、工具配置状态、模型数量和官方卡片转交；`node --check internal/console/web/app.js` 通过。此处是组件逻辑验证，不代替主任务随后进行的真实浏览器截图检查。

## 本地反代服务与紧凑套餐卡片

- 本地 API 服务已经具有真实启停、独立客户端密钥、文字/图片账号池、模型授权、HTTP 统计及显式 LAN/WSL 访问设置。新增卡片直接显示状态、地址复制、启用客户端数、配置账号数、在途请求和本次运行的 HTTP 次数；这些不是 Token 账单或完成任务数。
- 重启后运行时授权不持久化，“待启动”提供明确启动按钮，不把 enabled 意图伪装成 running。停用/启动跨卡片和弹窗互斥，异步状态读取有代次保护；失败显示状态待刷新，不显示旧绿色运行状态。
- 卡片成员与执行层一致：非空显式文字/图片池不隐式包含主锚点；各自为空才回退主账号。停用客户端和已知只查额度的 Web Session 不算 active。`active` 仍不承诺上游有效或服务正在运行。
- API 地址可复制；新建/重置客户端密钥默认遮挡，提供显隐与复制，关闭清空临时 DOM。服务端仍只持久保存摘要，不提供旧明文恢复接口，也不会拿账号原始 token 当客户端 Key。
- 独立复核发现并修复：保存和重置的并发请求会让新 Key 被旧回包淘汰；POST 成功后等待两个列表 GET 也可能在读取失败时丢失一次性 Key。现在客户端写入共用互斥，新 Key 在其所属弹窗直接呈现，无需任何额外 GET 成功。
- 复制降级路径使用离屏临时节点，并通过 finally 清空移除；复制返回 false / 抛错不会显示成功。不会留下带明文的隐藏 textarea。
- 官方账号的 Plus / Pro / Business 等套餐移到右上显眼徽章，状态放下一行。已验证套餐与导入提示区分，未知不冒充 Free；紧凑化间距和工具配置，保留 Salcara 原有浅色风格。
- 不新增没有真实实现的“实例网关”“OAuth 绑定”按钮，不把普通 API 余额、每日 Token 未知值填为 0。账号卡片的加入/管理入口走真实现有配置，不自动改账号绑定。

## 导入前认证检查与筛选

JSON / Token / 文件预览显示逐账号勾选列表，支持“检测可用性”“剔除失效”“仅保留已通过”。检测仅查询认证与额度，不发起模型推理或生图；结果不是所有模型的调用成功保证。失效、额度受限、待刷新、网络暂未确认及仅查额度分开显示；默认不自动丢弃未知账号。

用户筛选只影响这次导入，不改原文件或现有账号。前端保留原始字节，导入提交原批次的固定 selection 索引；检查中不能提交，关闭、改格式或换文件后迟到检查不污染新预览。导入原始 JSON 仍须通过严格格式检查，不静默清理损坏的 JSON。

### 导入检查后端合同

- `POST /api/local/codex/import/check` 接受原有完整 `credentials` / `files`、`format` 和可选 `replaceId`，返回 `{accounts:[{index,name,mode,status,code,plan}],checkedAt}`。索引始终对应严格解析后的原始批次，网络完成顺序不会重排记录。
- 只调用服务已有的可信认证/额度查询器，不调用模型、不导入 vault、不更新现有账号或额度缓存、不刷新或轮换 Token。成功仅证明本次查询已认证；它不是套餐内所有模型可用的证明。
- `verified` 表示认证成功；`limited` 表示明确的通用额度限制；`invalid` 仅用于没有可用刷新路径的 401；`needs_refresh` 保留仅 Refresh Token 或需要刷新的账号。403、429、身份不匹配、超时、网络错误均为 `unknown`，不能当作永久失效删除。Web Session 成功只返回 `quota_only`，不因此获得 API 执行权限，也不调用 CLI 刷新器。
- 套餐只输出归一化 family / source / verified，不返回原始上游文本、错误正文、账号 ID 或凭据。显示名称在截断前对整批凭据脱敏，避免另一条长 Token 被截断成可见前缀。
- 每批最多 64 条，整个服务最多同时检查 4 批、对上游最多 3 个并发请求。第 5 批立即拒绝，控制台返回固定的 503 重试提示。单请求最多 5 秒、单批最多 20 秒；完成、取消与关闭均释放准入资源，服务关闭会取消检查。网络期间不持有凭据锁。
- `POST /api/local/codex/import` 新增可选 `selection:[原始索引]`；省略保持原来的全部导入。空数组、null、重复、越界、负数、非整数均拒绝。选择顺序不会改变原始账号顺序；先严格解析全批，不能靠取消勾选掩盖损坏 JSON。预览/检查不接受 selection，避免二次索引映射。
- 替换账号时原始全批必须恰好一条，不能用 selection 绕过限制。导入仍使用既有持久化与显式刷新流程；筛选不会删除磁盘文件或已有账号。

实现文件：`internal/codexaccounts/import_check.go`、`service.go`、`internal/console/official_import_check.go`、`official_accounts.go`；回归文件为两个 `import_check_test.go`。13 个测试函数（含表驱动子用例）覆盖认证分类、只查额度、可信 HTTP 路径、错误脱敏、无写入/无旋转、全局并发、批次准入和释放、取消/关闭、超时、原始索引/乱序返回、选择边界、原始批次替换限制及非法 JSON 不被静默略过。全部使用合成数据及本机测试 HTTP 服务。

后端最终验证：最后批次上限及长凭据脱敏补丁之后，定向 `go test ./internal/codexaccounts ./internal/console -run 'TestImportCheck|TestPreparedImportSelection|TestOfficialImportCheck|TestOfficialImportSelection' -count=1 -timeout=180s` 通过（0.576s / 1.191s）；两包完整 `go test -race ./internal/codexaccounts ./internal/console -count=1 -timeout=600s` 通过（12.526s / 43.290s）；两包 `go vet`、局部 diff check 与新增文件尾空格检查通过。未使用真实账号验证，也未发布或部署。

前端检查包含账号/服务卡、一次性 Key、复制降级、图片/网络设置及自动 API 表单六个脚本；新增卡片启动按钮渲染后合计 87 条通过（1046ms），两个 JavaScript 语法检查通过。启动交互链路另行验证，不把组件合成检查描述为真实账号端到端验证。

## API 编辑器自动设置

后续 API 卡片直接启动及来源确认链路见 [API-CARD-LAUNCH-20261012.md](API-CARD-LAUNCH-20261012.md)；本地服务弹窗简化、原子密钥重置与首建防重复见 [LOCAL-API-SIMPLIFIED-20261012.md](LOCAL-API-SIMPLIFIED-20261012.md)。这些文档分别记录最终验证范围，不将 UI 模拟测试当作真实账号验收。

按用户最新要求移除“服务商认证方式”“接口类型”控件。新建及更换基础地址时，接口走现有按模型自动规则；认证对精确 Anthropic 官方域名使用 x-api-key，其余使用 Bearer。Go 的空认证默认也采用相同规则，并保留显式指定值。

编辑已有 API 的名称/密钥时，基础地址不变则保留已保存的认证和接口类型；标准 /v1 与尾斜线视为同一基础地址，避免旧的 GPT Chat-only 或自定义认证被隐式覆盖。换源才重置自动默认。此处不是通用网络协议探测，也没有在生成请求上做盲目重试；未知自定义厂商仍须符合受支持协议。

浏览器检查另发现关闭弹窗先移除 DOM 时普通查询无法清除已分离的密钥输入框。现在显式跟踪临时 input 引用，在销毁时清空再释放；有独立回归覆盖，不持久保存明文。

## 套餐细分调查（尚非已实现映射）

2026-10-12 按用户要求实际检索并打开官方英文页面：[Pricing](https://learn.chatgpt.com/docs/pricing) 当前列出 Pro $100 / $200 / $500；[Meet dots](https://learn.chatgpt.com/docs/dots) 另提到 Business Premium；[User lifecycle](https://learn.chatgpt.com/docs/enterprise/user-lifecycle) 区分 Teachers / Healthcare 与 ChatGPT / Codex-only 席位。套餐、计费方式、席位、权限和 API 账单不能混为一种档位。

本地 `UsagePlan.Raw` 保留原始套餐标识，但卡片按 family 展示，因此 `prolite` / `promax`、Business / Enterprise / Edu 的多个标识被合并。公开页面未建立这些内部值与上述价格的一一映射，不能从名称或剩余额度猜价格。此次只完成调查，未把猜测映射写入执行策略，也未声称所有套餐细分已展示。
