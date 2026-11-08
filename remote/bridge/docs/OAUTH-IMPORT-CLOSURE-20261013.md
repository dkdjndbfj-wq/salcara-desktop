# OAuth 与账号导入收尾（2026-10-13）

本轮负责浏览器/设备授权入口、手工回调、Token/JSON 导入及相关界面竞态。对照本机只读的 Cockpit `be24ce6171279741c2cd141bc02bd9f380f4b67c`；账号卡片、额度费用、清理和托管启动由协作对话负责，其结果见 [账号卡片与费用记录](ACCOUNT-CARDS-BILLING-CLOSURE-20261013.md)。

## 完成的修复

- 浏览器回调端口 1455 不可用时回退 1457。保留实际 listener，授权链接、手工回调与 Token exchange 使用同一 redirect，避免探测后重绑的空隙。两个端口均不可用时给出简短设备授权提示，测试 fixture 不隐式回退到生产端口。
- 等待 listener 也计入授权期限；显示真实 context deadline。明确取消、过期、端口不可用使用固定分类，终态清除授权信息，原始系统/上游错误不进入 UI。
- 回调成功响应先有界排空，再关闭连接。修复前压力测试五轮中三轮出现账号成功但浏览器 EOF；修复后同一测试通过。
- Token 响应读取超时保留过期分类。即使上游响应完整、reader 在过期后返回正常 EOF，也检查 context，禁止保存迟到凭据。
- 支持 `headers`、`custom_headers`、`customHeaders` 中的 Authorization 与 ChatGPT-Account-Id，保留明确工作区，拒绝相互冲突，任意附加 header 不进入执行配置。
- API Key、嵌套及平铺 Agent Identity 与 OAuth 混入时拒绝。逐一核对别名，空值不能遮蔽另一个别名中的实际凭据；正常导出的 null/空白占位仍兼容。
- 旧登录完成后的列表刷新不能关闭或取消同一弹层中新开的浏览器/设备授权，新授权请求发起时即使尚未返回 session ID 也受保护。列表刷新失败会重试，旧轮询/旧弹层结果不能覆盖新状态。
- 取消成功后立即移除旧授权链接和设备码，再查询真实终态。授权先完成时正确显示成功并关联账号；查询短暂失败时保留同一会话重试，不误报“已取消”。
- 关闭授权弹窗、切换弹窗或登录启动响应晚于关闭时，也后台取消并核对真实终态。已完成的账号经既有身份检查关联，再重读列表；短暂失败最多重试两次，不覆盖新弹窗。删除或替换后的身份冲突终止收尾，迟到读取不能恢复旧卡片。
- 兼容 Cockpit Sub2API access-only 导出中同一个 `expires_at` 同时为根级 Unix 数字与内层 ISO 字符串的情况。仅此字段按精确时刻比较，保留毫秒；不同时间、账号/邮箱/套餐的类型冲突仍拒绝。
- 支持 `personal_access_token` 授权模式及 `auth_mode` / `authMode` / `openai_auth_mode` / `openaiAuthMode`。任一层或别名声明 PAT 时，最终必须有 Access Token 且没有非空 Refresh Token / ID Token；API Key、Agent Identity、未知模式与冲突凭据仍拒绝。
- 导入提交后清除原始凭据，仅保留安全结果状态。保存成功但读取失败时显示“账号已导入，列表刷新失败，请重试”；刷新只重读列表，不重新发送凭据。响应丢失时明确结果未确认；旧完成/错误不能覆盖新预览。账号页需同时读到有效的 API 和密钥库列表，失败、格式异常或被更新读取替代时均不误报刷新成功。

原有浏览器/设备码选择、打开/复制授权链接、设备码复制、完整回调粘贴及多文件导入入口保留，沿用现有控件和样式。

## 多轮复核与验证

两名子代理先分查授权和导入，再交换审查前后端生命周期与导入边界；主代理补 HTTP 契约验证并集成。发现的竞态使用 deferred UI 请求、真实本机 callback 或受控 response body 先复现，再验证修复。

第二阶段追加兼容和关闭/导入恢复修复后的验证结果（替代本文件原 84 项 UI 与普通/race 耗时统计）：

| 检查 | 结果 |
| --- | --- |
| `go test ./internal/codexaccounts ./internal/console -count=1 -timeout=600s` | 通过；15.253s / 52.177s |
| 同两包 `go test -race ... -count=1 -timeout=600s` | 全部通过；17.438s / 75.173s |
| 第一阶段 OAuth 回调、读取超时、迟到完整响应、取消清理的定向 `-race -count=5` | 通过，12.827s；含 1280 次实际本机 HTTP 回调；第二阶段未改该连接器 |
| 第二阶段 `Import\|Portable` 定向普通与 race | 通过；1.099s / 2.689s |
| 账号卡片、OAuth errors/lifecycle/close、import-result 五份 Node UI 测试 | 105/105 通过，无跳过 |
| `go vet ./internal/codexaccounts ./internal/console` | 通过 |
| `go build ./...` | 通过 |
| 本轮 Go 文件 `gofmt -d`、账号 JS `node --check` | 通过，无格式差异 |
| Playwright / Edge 本机合成导入流程，1280×720 与 390×844 | 确认只提交 1 次导入，失败后恢复共读列表 2 次；窄窗口无横向溢出、凭据输入已清空 |

HTTP 集成回归覆盖取消晚于凭据提交：后续 status 仍返回 complete、保留同一账号身份，并完成受保护卡片关联。已有设备取消回归覆盖取消先完成的路径。旧设备 UI fixture 同步为取消后返回 cancelled，未修改生产取消接口语义。

第二阶段新增 11 项关闭授权、10 项导入结果 UI 回归，以及 `import_reference_exports_test.go` / `import_reference_pat_test.go`。真实参考差异、关闭完成竞态及列表读取失败/过时均先复现再修复；两名子代理交换只读复核后未发现新增可复现缺陷。参考文件为 Cockpit `src/utils/codexExportFormats.ts` 与 `src-tauri/src/modules/codex_account_tests_identity_import_refresh.rs`；仅按协议与行为独立实现。

可复跑 UI 命令：`node --test scripts/test-codex-account-ui.mjs scripts/test-codex-oauth-errors-ui.mjs scripts/test-codex-oauth-lifecycle-ui.mjs scripts/test-codex-oauth-close-review-ui.mjs scripts/test-codex-import-result-ui.mjs`。本机截图位于 `output/playwright/import-saved-refresh-failed.png` 和 `output/playwright/import-saved-refresh-failed-narrow.png`；该 fixture 只模拟导入与列表请求，不代表真实 OAuth 或手机原生链路验收。

## 验收范围

上述验证只使用合成凭据、本机 HTTP 服务和注入 transport；本轮未进行真实账号授权、额度请求、付费推理、打包部署或发布。真实浏览器/设备授权、上游对备用 redirect 的接受、套餐额度及目标安装环境仍需用户实际验收。

导入契约仍区分 legacy Codex CLI OAuth、仅查额度的网页 Session、API Key 和 Agent Identity；后两类不会被当成 OAuth 导入。未实现新版动态 SIWC Agent Identity 流程，不能将此记录表述为整个 Cockpit 功能全部对齐。更完整的范围与差异见 [功能复核表](COCKPIT-PARITY-REVALIDATION-20261011.md) 和 [账号模块契约](../internal/codexaccounts/README.md)。
