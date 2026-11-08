# 远程来源保护审计（2026-10-10）

2026-10-11 增量记录：已找到并使用既有便携 GCC，Hub 全量竞态检测通过；Bridge 的全部有测试包分组竞态通过，并对随后合入的对话/安装器改动复验。本文末尾“缺 C 编译器”的描述是当时的历史状态，当前功能与验证边界见 [最新复核](COCKPIT-PARITY-REVALIDATION-20261011.md)。真实账号、来源证明、桌面与 Android 链路仍未完成验收；CodeRabbit 的 Windows 限制未被普通检查代替。

本轮只改桌面 Bridge、Hub 策略和协议内核；没有修改桌面端/手机端 UI、文案或动画，也没有发布版本。

## 已实现

- 每次 Bridge 启动生成内存租约种子。`remote-*` 的 HMAC 凭据绑定目标路由、所选账号、原始/有效配置指纹、中转站与手机绑定。
- 本地 gateway-backed 工具也绑定当前进程租约：`ToolConnection` 和 `/gateway/{codex,claude,...}` 同时使用 `GatewayCredential()`；Bridge 运行时旧的持久 `GatewayKey` 直接返回 401。`reverse_proxy`、`oauth`、`official_account` 来源强制经过网关，因此旧配置、重启前配置和其他程序直接读取的持久 Key 不会继续调用已保存的上游。未启动 Bridge 的纯库/测试构造仍可显式使用兼容回退，但生产入口在启动时总会生成新租约。
- 原生 companion 的新激活描述、来源身份与本次管理启动记录只是有限生命周期检查，不足以证明实际 provider/PID。不能据此承诺外部重启、外部配置变更已全部阻断；`restricted` 的原生发送/审批因此保持拒绝，`open` 保留兼容。
- 远程命令 handle 必须等于实际 managed account，而非任意已保存的 API 卡片。Codex/Claude 的一次授权调用读取不可变 Config 快照，网关还会验证该快照的凭据是否仍然有效；未解析、已删除、不同来源或本机修改后的旧快照不会偷偷转到新 Key。
- 远程池暂不替换账号、不联合其他成员目录；本机池仍可调度。恢复远程池前必须完成逐成员来源授权。
- Hub 来源策略在回执持久化和发往 Bridge 前再次检查；Bridge 在配置任务锁内再次校验来源。命令的 agent/tool/sessionKey 命名空间、control surface、API handle、来源类型、来源地址和 API 指纹必须一致。
- 受限来源的审批必须带会话归属；桌面审批允许携带 API handle；跨 Agent 的审批不会再按全局 approvalId 猜测。
- Bridge-owned Codex/Claude 的远程审批按完整 `sessionKey + approvalId` 核对并原子消费，不能在同一 Agent 的 A 会话里回答 B 会话。执行 worker 的配置签名也必须匹配授权快照；关闭/退出的 worker、缺少会话归属和只支持 ID 的旧适配器拒绝远程回答。签名与消费检查由同一 Agent 生命周期锁保护；本机旧审批路径保持兼容。
- API/中转站切换的活跃 SSE、备用站短轮询和最终持久化均校验来源；不再信任 `Phone` 布尔字段来跳过校验。已提交的同 operation 重试仍是回执读取，不重复切站。

## 关键边界

- `open` 表示 Hub 不要求签名 grant；Bridge 仍绑定远程命令与当前 managed API，而不只是可解析 handle。
- 进程租约是启动者/重启边界：持久 `GatewayKey` 不是生产 gateway 的授权凭据；每次 Bridge 启动都只接受新的内存 lease，单实例监听和 lease 轮换共同使旧工具配置 fail-closed。当前 loopback HTTP 协议无法证明同一 Windows 用户下的真实 PID/父进程；持有当前 lease 的本机程序仍可模拟客户端，生产隔离仍依赖私有配置 ACL 和独立用户账户。
- 独立 `/codex-api` 客户端 Key 仅作为持久摘要保存，`CodexAPI.Enabled` 不等于服务已运行；服务还需匹配当前进程 lease。启动/轮换会清掉内存绑定，控制台状态以 `running:false`/`requiresStart:true` 表示待显式启用，旧客户端和外部直连因此 fail-closed。
- `restricted` 需要可信 issuer、grant 与实际签发获取链路。目前后两者的生产闭环未完成。`ConfigRevision` 使用原始 vault `AccountFingerprint`，不是临时适配后的 worker 指纹；它包含凭据的哈希输入，不能将其当作任意可公开的业务字段。后续应改用不泄露凭据关系的 opaque/HMAC revision。
- Bridge-owned worker 不会因本机已安装 CLI 或存在外部配置就自动获取远程权限；原生 Desktop 的 provider 证明限制仍如上所述。这套机制不能抵抗同一 Windows 用户下可读取 Salcara 配置、修改进程内存或伪造已授权插件的恶意程序。生产部署应使用独立系统账户、ACL 和最小权限。
- 未进行真实多站点、手机后台、原生 Desktop 或 OAuth 账号验收。测试通过不代表全项目商业化验收通过。

## 本轮修改范围

`hubclient/source_guard.go`、`dispatch.go`、`standby.go`、`station_switch.go` 统一来源与执行账号校验；`config/connection.go` 与 `console/server.go` 生成/验证源绑定网关凭据；`agents/settings.go`、`codex.go`、`claude.go` 使用授权配置快照；`agents/agents.go`、`questions.go`、`util.go` 实施会话/worker 归属审批；`gateway/gateway.go` 禁止远程池替换并新增原生 Responses compact 透传。本次续修没有改手机、桌面前端、安装包或 Hub 源码/部署。

## 验证命令

```powershell
go test ./...
go vet ./...
```

新增回归覆盖：声明 A/执行 B、Phone 标志绕过、交接目标指纹、IPv6 origin、快照隔离、路由/账号/手机/站点凭据隔离、远程池不换成员、请求校验后变更账号（上游调用为零）、受限原生能力拒绝。

2026-10-10 续修增加：

- `agents/approval_snapshot_test.go`：同 Agent 兄弟会话不能串审批；授权 B 不能回答 worker A 的审批，授权 A 的不可变快照仍可回答；缺会话/已退出/关闭中的 worker 不消费请求；旧 adapter 本机审批保持可用但不能降级远程归属检查。
- `hubclient/station_source_regression_test.go`：14 个分项，覆盖活跃站与备用站缺少/过期证明、合法 restricted/open 切换、网络探测中 ProviderID/SourceType 变化的最终拒绝、同 operation 回执重试保留待上传事件。
- `hubclient/approval_scope_dispatch_test.go`：8 个 restricted/open 分项，覆盖 Dispatch 保留完整会话与回答、兄弟会话拒绝后仍可正确回答、其他已保存 API 的有效证明不能替代实际执行账号、旧 ID-only adapter 只保留本机兼容、远程缺会话直接拒绝。配置快照内部无公开读取器，worker 快照语义由 agents 的独立测试验证，不用测试伪造私有 context key。
- `gateway/compact_test.go`：原生请求/结果字节和 opaque 压缩状态不改写，跨协议和尚无执行器的 OAuth 来源拒绝；模型规则、大小限制、来源变化、超时和错误脱敏均覆盖。测试全部使用本机模拟上游，不调用真实付费模型。

验证状态：桌面 Bridge `go test ./... -count=1 -timeout 180s`、`go vet ./...`、`git diff --check` 已通过；追加 Dispatch 测试后又完成最新树的 `go test ./internal/hubclient -count=1 -timeout 90s`（9.700s）及 `go test ./... -timeout 180s`（agents 90.281s，hubclient 11.502s）复验。Hub 全量 tests/vet 在本次续修前已通过且未改其源码。竞态检测未运行成功：`go test -race` 要求 cgo，但本机 `CGO_ENABLED=0` 且没有 gcc/clang。确定性竞态注入测试通过不等于 race detector 通过。CodeRabbit 安装器不支持本机 Windows Git Bash 平台（`mingw64_nt-10.0-26200`），未完成 CodeRabbit 审查，独立只读代码复查不能冒充它。真实多站点/手机后台/原生 Desktop 验收仍未完成。
