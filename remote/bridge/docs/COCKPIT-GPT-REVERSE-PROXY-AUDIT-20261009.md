# Codex API 模式与 GPT 反代核对清单（2026-10-10）

2026-10-11 复核：本文保留实施前调查记录，官方导入、登录刷新、受保护执行器和独立 API 服务已在后续实现；当前完成/部分/blocked 状态及本轮旁路修复以 [最新逐项矩阵](COCKPIT-PARITY-REVALIDATION-20261011.md) 为准，不应继续将本文“未实现”条目当成当前代码状态。

> 历史调查基线：本文保留实施前的差距和决策依据，其中“未实现/未实施”不是当前状态。2026-10-10 的实现、复测结果和仍存在的限制以 [实施交接报告](CODEX-OFFICIAL-ACCOUNT-API-IMPLEMENTATION-20261010.md) 为准。

本清单描述 Salcara Bridge 的实际能力，不把计划当作完成。普通桌面/手机 UI、文案、动效没有修改。现有 API 库保存电脑端账号；目前是新增/编辑，不是 Cockpit 格式文件导入。

## 核对基线与使用边界

- Cockpit Tools 本轮核对提交：`be24ce6171279741c2cd141bc02bd9f380f4b67c`，版本 `1.3.66`。
- 其应用声明 CC-BY-NC-SA-4.0，README 还列出商业使用限制；不能把应用、UI 或其 sidecar 管理源码直接搬进 Salcara 商业项目。本轮只研究能力，新增代码独立实现。
- 底层 CLIProxyAPI 的许可独立于 Cockpit；Salcara 已有 MIT CLIProxyAPI/v7 translator 依赖，不等于集成了其认证/执行器。
- Cockpit 旧 handoff 说有 sidecar/legacy 两种模式；当前 `collection_gateway_mode` 已把两者都映射到 sidecar，旧 WS 代码保留作测试参照，不能只照旧文档推断现状。

## “Codex API 模式”要分清两件事

1. **工具使用 API**：Codex 的 provider 指向某个 URL 和 Key。这条静态 API 路径 Salcara 已有。
2. **账号输出成 API 服务**：将用户明确导入/授权的 Codex 官方账号，或已保存的上游 API，通过本机认证网关提供给工具调用。Cockpit 的 Codex API Service 属于这个方向，不是手机/Hub 服务，也不只是修改工具配置。

Salcara 当前网关只接受 loopback，本轮补的 `/v1/responses/compact` 是第二种服务的一个原生协议能力，**不代表官方账号已能反代**。目前没有把网关公开到公网，没有为外部应用发行客户端 Key，也没有自动读取本机登录文件。

值得独立借鉴的产品组合是：统一账号库 → 本机 API 服务 → Agent 工具引用。手机仍只用 opaque handle，Hub 仍只传任务和校验来源；不能把 OAuth token、上游 Key 或本机网关 Key 当成手机配对密钥。

服务状态、客户端 Key、账号池、Hub 来源策略必须分开：一个账号能用模型，不代表获准远程；一个客户端 Key 被撤销，不应该退出官方账号；停用 API 服务应恢复 Salcara 曾接管的工具配置，而不是删会话。

## 已核对并落地

| 能力 | Salcara Bridge 当前行为 |
| --- | --- |
| 多账号 | 每个 `LocalAccount` 独立保存 URL、认证方式、Key、模型目录和来源元数据；默认旧账号仍是手动账号。 |
| GPT 反代来源 | `sourceType=reverse_proxy` 用于电脑上配置好的 GPT/第三方反代；`providerId/providerName` 只保存卡片识别信息，不把 Key 发到 Hub。 |
| 账号池 | 本机 `poolEnabled + poolGroup` 显式加入池；远程 `remote-*` 固定所选账号，尚不允许仅凭 A 的来源证明转到池成员 B。 |
| 选择算法 | 模型目录过滤 → 健康的最高优先级 → `inflight / weight` 负载 → 稳定顺序；首选层冷却/满载时才降级到健康备用层；有会话 ID 时保持会话粘性。 |
| 并发和冷却 | 每卡可设 `maxConcurrency`；上游 401/429/5xx、超时、截断流会触发有界冷却，成功后清除；`Retry-After` 最高 5 分钟。 |
| Key 轮换 | 选择和转发前重新校验账号指纹；当前流不换 Key。旧 Key 的迟到结果不会污染替换后的同一账号 ID。 |
| 模型目录 | 本机池返回合格成员目录联合；远程只返回当前所选账号目录。目录与允许/拒绝规则同时约束请求。 |
| 模型规则 | `modelAllowlist` 和 `modelDenylist` 在目录和请求两处执行，拒绝规则优先。 |
| 协议转换 | GPT、Grok、Claude 等模型按模型族选择 Responses / Chat Completions / Anthropic wire；本地 Loopback 网关只接收本机工具请求。 |
| 连接稳定性 | Codex WebSocket 在建立后固定账号，并持续验证账号指纹；删除或换 Key 只影响新连接。 |
| 卡片诊断 | `GET /api/local/accounts` 返回来源、服务商、模型数量、是否需要网关、池成员、并发/冷却/最近使用等脱敏字段；不返回 Key。 |

多个 `poolGroup` 同时存在而当前 Agent 没有明确分组时会 fail-closed，不会把 A 中转站和 B 中转站的 Key 混到同一次路由；只有单一分组才自动使用。

## 与 Cockpit 的差距及实施顺序

| 能力 | Cockpit 已有 | Salcara 状态 / 后续边界 |
| --- | --- | --- |
| API Key / provider 模式 | 内置 OpenAI 与自定义 provider，配置投影 | 已有静态 Key、BaseURL、Wire、模型目录及原工具备份恢复；本轮补原生 Responses compact 透传，不跨协议伪造压缩。 |
| 官方 OAuth | 浏览器/设备授权、id/access/refresh token、失效分类 | 未实现。必须新增真实 token bundle、过期状态、刷新单飞、rotation 原子落盘及 SDK 执行器；来源标签不代表登录成功。 |
| 本机/JSON/批量文件导入 | 有实际导入实现 | 尚无导入路由。计划版本化预览、去重、数量上限、原子提交；默认导出脱敏元数据，凭据导出另设明确操作。 |
| 服务生命周期 | sidecar 配置指纹、readiness、退出监控、有限恢复 | 嵌入式网关随 Bridge 运行；缺独立 enabled/running/baseURL/lastError 合同。保留当前内核，不为模仿而额外启动服务。 |
| 额度、套餐、到期、刷新 | 有真实接口与错误状态 | 未接数据源，不返回猜测额度，也不将模型列表刷新叫作 OAuth 刷新。 |
| 命名客户端 Key、模型别名、请求统计 | 有规则与用量存储 | 尚缺生命周期、脱敏审计与持久化统计；需同时约束目录和真实请求，不只做卡片。 |
| 图片/音频专用路由 | 有独立接口、任务槽与限制 | 不能把现有文本 translator 当作全模态支持；须分别验证输入、输出、取消、超时与恢复。 |
| 手机远程 | 本轮不拿本机账号切换工具推断其远程能力 | 需要 Hub 来源合同、Bridge 快照与执行器三者一致；配对成功并不证明上游账号可用。 |

## 尚未完成

1. `oauth` / `official_account` 目前只是来源分类，尚未实现 OAuth refresh、官方账号登录文件导入、过期时间和配额保留。不能把本机登录状态自动当成可反代账号。
2. Hub 已有 `open/restricted/disabled` 与签名 grant 校验，Bridge 已有执行端二次校验；可信 issuer 的签发、来源验证与手机获取 grant 的实际闭环尚未完成。CLI 存在或 URL 相同不能作为本站授权证明。
3. 原生 Desktop 当前不能证明进程真正采用了哪一份 provider。`restricted` 原生发送/审批保持拒绝；`open` 为明确的兼容策略，不将其宣传成可信来源证明。
4. 账号池是本机调度器，不负责 Sub2API 计费、余额、套餐或跨用户 API Key；尚不承诺 OAuth、图片/音频与多模态字段无损转换。

## 验收重点

- 同一账号换 Key：旧流继续使用旧连接，新请求只使用新指纹；旧请求返回失败不会让新 Key 进入冷却。
- 两个池成员各自拥有不同 GPT 模型：模型目录显示联合结果，实际请求只发送给拥有该模型的成员。
- 高优先级账号并发已满：选择同组内其他健康、支持该模型且有容量的成员（包括明确加入的低优先级备用成员）；所有合格成员不可用时返回明确 409，不回退到未加入池的 Key。
- 上游断流：客户端收到错误事件，不伪造完成；账号进入有界冷却，成功请求后恢复。
- 手机切换 Agent/API：只改变后续 Bridge 请求的账号选择，不重写原桌面工具的历史或当前流。

## 下一阶段的独立实现顺序（未实施）

1. 官方账号凭据合同与版本化导入预览：明确用户授权、去重、限制数量、确认兼容格式；凭据只在电脑端保存，默认导出脱敏。
2. OAuth 登录/刷新与真实执行器：刷新单飞、轮换版本比较、原子保存、失效分类；撤销/删除期间迟到刷新不得复活账号。不能将 access token 填进普通 API Key 卡片便宣称完成。
3. 本机服务生命周期：独立 `enabled/running/baseURL/lastError/generation`，启动就绪再接管配置；配置变化不能随意中断活跃流，失败保留可恢复备份。
4. 客户端 Key 与路由规则：Key 的身份/撤销/账号范围/模型规则独立于上游凭据；默认仅本机，LAN 暴露需用户明确开启。模型别名同时约束目录与请求，不能只改显示名。
5. 套餐、额度、到期与统计：仅显示真实接口数据，记录脱敏 request ID；区分鉴权失效、额度不足、并发拥塞、暂时故障，不在不确定已执行的请求上自动重试付费任务。
6. 手机远程：先完成可信 issuer 的签发/获取闭环与实际执行来源证明，再开放逐成员授权的远程账号池；不能把本机池默认授权给手机。

先完成 1–3 才能将功能称为“官方账号 Codex API 服务”。图片/音频、WebSocket 恢复与其他 Agent 执行器分项验收，不能从文本请求成功推断全模态兼容。

## MIT SDK 路线的可行性与接入陷阱

已核对当前已安装的 CLIProxyAPI/v7 `v7.2.157` 源码，不需要复制 Cockpit glue：

- 公共配置入口是 `sdk/config`，不能沿用部分旧 SDK 文档的 `/v6` 或直接导入其 `internal/config`。
- `sdk/cliproxy.NewBuilder()` 可配置 config、configPath、core auth manager、server options 和管理密码；`Build()` 返回 Service，`Run/Shutdown` 负责执行器、监听、watcher 与自动刷新。现有 Bridge 只导入 translator，尚未接入这些能力。
- `sdk/auth.NewCodexAuthenticator`、`sdk/auth.Manager.Login` 是实际授权入口；`sdk/api.NewManagementTokenRequester` 提供 token 请求/授权状态/callback 的有限管理面。管理接口必须保持本机认证，不直接透传给 Hub/手机，也不沿用内置控制台代替 Salcara 的权限边界。
- SDK 的 `OnAfterStart` 在固定 100ms 等待后调用，不能把 callback 或“进程存在”当作 readiness；宿主要执行认证就绪探测，再接管工具配置。
- 当前 SDK `Run` 在启动时创建 30 秒 shutdown context，长时间运行后 deferred shutdown 使用的是过期 context。接入时应以新建的超时 context 调用 `Shutdown`，再取消运行；需覆盖长期运行、重复停用和退出过程中保存凭据的测试。
- SDK 的默认文件保存和运行时更新不能被当作已满足产品的持久化事务：需要独立原子写入 store、Windows ACL/凭据保护、可观察的保存失败；进程内刷新锁不等于跨 Cockpit/原 Codex/Salcara 多进程互斥。必须明确一个凭据更新所有者，避免共享可轮换 refresh token 被多个程序同时刷新。

可选方案是独立编写、由 Salcara 管理的 Go 账号执行服务，按需启用而非每个 Agent 各起一个。既有 Bridge 聚合网关继续承接工具和远程请求；不能仅将一个可访问全账号池的服务 Key 保存成“账号 A”，否则 A 的远程授权又会绕到 B。接入前必须证明请求范围能固定到具体底层账号，或在范围不明确时拒绝 restricted 远程执行。是否嵌入 Bridge 还是独立进程，要通过启动/退出/刷新故障测试后决定，不为模仿而重写现有内核。

## 对照资料

- [Cockpit 许可声明](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/src-tauri/Cargo.toml)
- [当前账号与 OAuth 数据模型](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/src-tauri/src/models/codex.rs)
- [实际 OAuth 与刷新流程](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/src-tauri/src/modules/codex_oauth.rs)
- [导入/导出实现](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/src-tauri/src/modules/codex_account_import.rs)
- [当前 sidecar 配置与运行模式](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/src-tauri/src/modules/codex_local_access_sidecar_config.rs)
- [API 服务 handoff（部分模式描述已过时）](https://github.com/jlcodes99/cockpit-tools/blob/be24ce6171279741c2cd141bc02bd9f380f4b67c/docs/CODEX_API_SERVICE_HANDOFF.md)
- [OpenAI 独立压缩与服务端压缩](https://developers.openai.com/api/docs/guides/compaction)
- [CLIProxyAPI v7.2.157 公共 Builder](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.157/sdk/cliproxy/builder.go)
- [CLIProxyAPI v7.2.157 服务生命周期](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.157/sdk/cliproxy/service_lifecycle.go)
- [CLIProxyAPI v7.2.157 OAuth 管理包装](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.157/sdk/api/management.go)
- [CLIProxyAPI 许可](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.157/LICENSE)
- Sub2API 的分组/组合模型路由：[COMPOSITE_GROUPS.md](https://github.com/Wei-Shaw/sub2api/blob/main/docs/COMPOSITE_GROUPS.md)
