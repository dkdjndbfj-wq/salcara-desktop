# Cockpit 风格本地账号池内核

2026-10-11 复核：本文件保留第一阶段池调度说明；后续已实现官方账号导入/刷新和执行器，仍需可信额度/套餐与受限来源连接器。当前完整对照以 [最新逐项矩阵](COCKPIT-PARITY-REVALIDATION-20261011.md) 为准。

状态：第一阶段已实现，未改桌面/手机 UI；OAuth 刷新连接器和来源授权策略仍需后续接入。

2026-10-10 修订：以下池能力只适用于本机路由。远程 `remote-*` 固定当前所选账号，直到逐成员来源证明落地后才能启用远程池/联合目录；A 的授权不能自动允许 B。

## 已实现

Salcara Bridge 原来已经有一个只绑定单个 `LocalAccount` 的 loopback 协议网关。本轮在这套网关上增加了可选的 Cockpit 风格账号池，不另起一个服务：

- `LocalAccount` 增加来源和路由元数据：`sourceType`、`providerId`、`providerName`、`poolGroup`、`poolEnabled`、`priority`、`weight`、`maxConcurrency`；
- 账号池只选明确标记为 `poolEnabled` 且有 `poolGroup` 的账号，旧账号默认仍是手动账号，不会因为升级自动切换；
- 反代/OAuth/官方来源即使只有一个成员也必须走本机 gateway；gateway 凭据绑定当前 Bridge 进程 lease，重启或外部直接启动旧配置不会继承账号池授权。
- 同一个 Agent/工作台通过 `poolGroup` 固定到一个账号池，避免把不同来源的账号混在一次路由中；
- 当本机同时配置多个账号池而当前 Agent 没有明确选择其中一个时，网关会拒绝混池并要求先指定 `poolGroup`；只有单个账号池时才自动使用它；
- 新请求按模型目录过滤，再按优先级、归一化负载（inflight / weight）和稳定轮询顺序选择；
- 账号池启用后，模型目录查询返回当前分组各合格成员的稳定联合目录；发送请求时仍按具体模型重新选择真正支持它的成员；
- `GET /v1/models` 在同一账号池内返回各成员已缓存目录的稳定并集，并逐账号应用允许/拒绝模型规则；因此 GPT 反代池中只有某一把 Key 支持的模型也能出现在目录里，实际请求仍会重新按模型选择对应成员；
- Claude Desktop 的不透明兼容模型路由会在账号池筛选前还原为真实上游模型，再执行目录和允许/拒绝规则，避免 GPT/DeepSeek 等非 Claude 模型在池内被误判为不可用；
- 有会话标识时启用有界会话粘性，默认 1 小时、最多 4096 条；模型不支持、账号被删除、账号冷却或并发已满时会解除粘性；
- 每个账号有并发上限、失败退避、`Retry-After` 上限和成功恢复；不会把 401/429/5xx 当成“换一个模型”或偷偷退回未授权账号；
- 账号池健康状态绑定账号指纹而不只是条目 ID：同一张卡换 Key 后不会继承旧 Key 的冷却，旧请求迟到的成功/失败回报也不会污染新 Key；
- 选中的账号在一次请求和 Codex WebSocket 连接内固定。编辑/删除/换 Key 只影响后续请求，当前流不会中途换 Key；
- 可选 `modelAllowlist` / `modelDenylist`，拒绝规则优先；池目录会联合所有合格成员的已缓存模型，避免高优先级账号的目录遮住备用账号独有的 GPT 模型；
- 账号池未找到可用成员时返回明确错误，不回退到未标记的普通账号；没有启用账号池的旧路径保持原有单账号行为。是否启用池由 Agent 当前已应用的账号绑定决定，仓库里存在其他池成员不会改变普通 Agent；
- 模型目录是只读路径：使用池的 Agent 返回稳定联合目录，不占用任何推理并发槽、不因单个成员冷却而隐藏其他成员模型；实际发送仍在发送前重新选择并固定到一个成员；
- 会话粘性按 Agent 路由、池分组和会话 ID 做命名空间隔离，Codex 与 Claude 不会因复用相同会话字符串而串池；池成员资格、分组和凭据指纹任一改变都会使正在建立的新请求失效；
- `GET /api/local/accounts` 返回脱敏的来源、服务商和池状态；新增 `POST /api/local/accounts/pool` 可单独启用/停用某个账号池成员，不会重启 Agent。
- 旧版卡片保存接口若没有提交这些新字段，后端会保留已有来源、账号池和模型规则，避免升级后旧 UI 编辑一张卡片就把池配置清空。
- `GET /api/local/accounts` 同时返回模型数量、允许/拒绝规则、是否需要本地网关和无密钥运行状态，供现有卡片按 Salcara 风格呈现；

示例请求（只使用占位 ID，不要把真实 Key 写入脚本）：

```json
POST /api/local/accounts/pool
{"id":"account-id-from-local-accounts","enabled":true,"group":"my-provider-pool"}
```

停用时：

```json
{"id":"account-id-from-local-accounts","enabled":false}
```

## 与 Cockpit / Sub2API 的对应关系

- Cockpit 的“本地认证网关 + 账号池 + sidecar 路由”对应本地 Bridge 的 `gateway.Handler + AccountPool`；协议转换仍使用已声明的 CLIProxyAPI MIT 许可 translator，不复制 Cockpit 的账号数据库或 UI。
- Sub2API 的“上游账号、用户 API Key、分组、模型路由、sticky session、并发/健康调度”被拆成 Bridge 的账号来源、`poolGroup`、模型目录和运行时选择器。Hub 仍只负责设备配对和远程策略，不保存这些 Key。
- `sourceType` 目前是来源分类和后续策略的接口：`api_key`、`oauth`、`reverse_proxy`、`official_account`。本轮没有伪造 OAuth refresh，也没有把本机登录文件自动认定为可信反代。

## 尚未声称完成的部分

1. `oauth` / `official_account` 目前只完成数据模型分类；真正的 token 刷新、过期时间、账号导入和安全存储还要接入专用 ProviderAdapter。
2. `reverse_proxy` 目前表示电脑端已配置的反代 API。它还没有获得 Hub 的本站来源证明；`restricted` 策略应等待独立的签名 grant/connector 验证器。
3. 账号池只做本地调度，不负责 Sub2API 的计费、余额和上游 OAuth 池；这些状态应由对应 ProviderAdapter 或 Sub2API 自己返回。
4. 旧的 `remoteSendSupported`、CLI 探测结果和 API URL 不能作为本站授权证明。Hub 已提供三种权限模式和签名 grant 合同，但 Bridge 仍需来源 connector 才能在 `restricted` 下执行；见 [远程 API 来源与访问策略设计](../../../../salcara-hub/docs/REMOTE-ACCESS-POLICY-DESIGN-20261009.md)。

## 验证

- `go test ./internal/...` 通过；包含原有 Codex/Claude 协议转换、WebSocket、控制台和 HubClient 回归；
- `go vet ./internal/...` 通过；
- 新增账号池测试覆盖手动账号隔离、会话粘性、模型过滤、优先级、容量、冷却恢复、Key 替换、池目录只读、跨 Agent 粘性隔离和成员变更复核；
- 未修改桌面/手机 UI、动画或文案；未使用真实 API Key、OAuth Token 或收费推理。
