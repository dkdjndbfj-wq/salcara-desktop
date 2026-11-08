# 2026-10-14 受限远程账号连接器复核与补齐

本记录纠正 10 月 10 日和 10 月 11 日文档中“受限 official/oauth/reverse_proxy 均未实现”的旧结论，不改写那些日期的验证结果。

## 当前可执行范围

- **受保护官方账号已经有生产授权闭环。** Bridge 从已认证的当前 Hub 取回冻结命令和 challenge，解析本地账号句柄，要求 `official_account`、`openai`、保护 vault 引用和固定 `https://chatgpt.com/backend-api/codex` Responses 路由。站点必须明确 `allowOfficialAccounts=true`。Bridge 只回传无 Token 的来源声明；Hub 在当前配对、策略和 challenge 下使用进程内 Ed25519 密钥签发 `station-official` grant。最终指令再次核对账号、配置修订、Agent、模型和站点；执行只能进入该账号的受管理 gateway。
- **本轮补齐 Claude Code 的官方账号路径。** 原有获取授权逻辑只允许 Codex，而实际 gateway 已能把 Claude Messages 转成官方 Responses。现在仅对两个已有生产路由 `codex` / `claude` 签发；Hub 也明确限定这两个内置受管理路由。模型允许/拒绝规则、账号身份 epoch、站点授权和最终 executor 的 `current` 检查继续生效。未给 Hermes 或原生 Desktop 声明实现。
- **本轮补齐中转站反代凭据的可信签发接线。** `reverse_proxy` 使用 `verified_proxy`，只接受唯一、明确注册该来源类型且 Agent、同源 HTTPS endpoint、完整 API base 都匹配的 issuer。仍由真实 issuer 验证该 Key 和真实服务端账号后签名，Bridge 精确检查所有签名字段。改一个本地来源标签、一个成功的模型列表或另一个站点的 Key 均不能代替签名。拒绝验证、来源混淆、跨域、不同租户路径、歧义 issuer、迟到签发结果都拒绝。
- **OAuth 不是另一条可随意声明的旁路。** 正常浏览器/设备授权或 Token/JSON 导入由 `console.attachOfficialRows` 关联为 `official_account`，引用实际 vault 的 `IdentityEpoch`。裸 `oauth` 标签、任意 OAuth 地址、网页 Session、伪造的官方标签不因此获得账号远程权限。实际 executor 还会检查 vault 中账号存在、epoch、可执行状态、刷新状态和 quota。

## 站点与凭据边界

API A 与 API B 的判断继续使用规范化后的完整站点 API base，不按 Key 是否相同判断。反代签发器的宽 scope 也不能扩大 Hub 操作者的独立站点策略。换账号、换配对或换站期间返回的旧签发结果不返回给手机；站点撤销后旧声明不能重新绑定，旧 worker Key 不能执行。

这里的“通过站点远程”是 **手机经所选 Hub 调度，由该站点明确授权电脑上的受管理账号执行**。模型请求由电脑上的受保护 executor 直连固定提供商；Hub 不持有官方 Token，也不转发官方推理流量。本地原生进程的任意外部配置或登录态仍不属于此证明范围。

## 验证与保留边界

新增合成测试串起 authenticated challenge → 来源校验 → 站点绑定 → gateway → 固定官方执行回调，覆盖 Codex Responses、Claude Messages 转换及实际结果、模型拒绝、撤销后的旧 Key、签发期间账号身份/配对/站点变化。反代测试覆盖可信签名和拒绝签名、错误类型/路径、跨域零凭据发送；Hub 测试覆盖 Claude/Codex 以及反代签名不能扩大站点范围。所有测试只用临时配置、合成 Key、本地 HTTPS fixture，没有真实 OAuth、付费推理或发布。

本轮验证结果（2026-10-14）：

| 验证 | 结果 |
| --- | --- |
| Bridge `go test ./internal/hubclient ./internal/gateway ./internal/config -count=1 -timeout 240s` | 通过，分别 60.854s / 1.003s / 1.898s |
| Hub `go test ./internal/hub -count=1 -timeout 240s` | 通过，14.148s |
| Bridge `go test -race ./internal/hubclient -run 'Test(Station\|Source\|StandbySource\|ActiveSource)' -count=1 -timeout 240s` | 通过，8.168s |
| Hub `go test -race ./internal/hub -run 'Test(Station\|Source\|StandbySource\|SourcePolicy)' -count=1 -timeout 240s` | 通过，4.839s |
| Bridge 上述三个包及 Hub `internal/hub` 的 `go vet` | 通过 |
| 修改范围的 `git diff --check` | 通过 |

竞态使用现有便携 GCC。这里的普通测试是上述受影响包的完整测试，竞态是来源/站点专项，不表述为最新整仓所有包的全量竞态。测试包无 `t.Parallel`；本地 HTTPS fixture 临时替换 `http.DefaultTransport`，并在每个串行测试结束时还原，未修改生产全局 transport。

仍需外部实现或目标环境验证的范围：

- 第三方站点需实现并部署真实 issuer；仓库中的合同不是已存在的 Sub2API 端点。`reverse_proxy` 的本机随意 URL 或缺少 issuer 的站点仍不可用。
- 原生 Codex / Claude Desktop 进程的实际 provider 证明及其任意外部启动接管仍未实现，保持拒绝；受管理 CLI 会话不受此原生限制。
- 新版动态 SIWC Agent Identity 不是此旧版 Codex OAuth 连接器的能力。
- 真实账号、真实站点部署与正式客户端手机到电脑链路尚需目标环境验收。

主要源码：Bridge `internal/hubclient/source_grant_client.go`、`source_guard.go`、`internal/config/station.go`、`gateway_authorization.go`、`internal/gateway/gateway.go`、`internal/codexruntime/account_handler.go`；Hub `internal/hub/source_challenge.go`、`source_policy.go`、`station_policy.go`。
