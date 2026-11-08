# API 卡片直接打开工具 · 2026-10-12

本轮按用户要求增加 API 卡片快捷打开入口，复用已有 Agent 确认弹窗和原工具配置。没有另开用户数据目录，没有调用 `/api/local/launch` 的独立实例路径，也没有重做 Agent 页布局。

## 前端实现范围

- `internal/console/web/workbench.js`：公开 `wbOpenAccount(target, id, button)`；支持 Codex Desktop、Codex CLI、Claude Code、Claude Desktop。
- `internal/console/web/claude-desktop.js`：既有 Claude 弹窗增加可选暂存上下文，继续使用自己的 status / switch 事务。
- API 卡片按钮及样式由主任务修改；下列启动控制不依赖按钮是否隐藏，入口和确认均重新验证可调用账户集合。

普通 API 可用于四个工具；已关联官方账号只可用于 Codex。`libraryOnly`、ChatGPT 网页会话仅查额度账号、失效的卡片 ID、未知工具均不能经本入口启动。不会从官方账号展示库冒充可调用账户。

## 用户确认与原工具数据

打开卡片先暂存选中的 API 和模型菜单覆盖选项，立即展示现有弹窗，同时读取最新本机账号/工具状态。目录刷新、覆盖开关和取消均不写绑定、不启动工具。

确认框默认未勾选，文案为“同系列工具任务已经结束”。这是用户显式确认，不宣称能自动判断外部工具所有任务是否结束。Codex CLI 与 Desktop 可能共享同系列原配置；没有把它们伪装成完全独立实例。

确认后：

1. 再读最新卡片和工具状态，检查来源版本、权限、安装状态。
2. Codex / Claude Code 执行既有 `/api/local/bind`，随后 `/api/local/switch`，`keepModel: true`。bind 途中关闭或导航会取消后续 switch；已保存的选择保留为待应用，不以回滚覆盖其他操作者的更新。
3. Claude Desktop 只执行 `/api/local/claude-desktop/switch`，携带 `catalogOverride`，不走普通 bind / CLI writer，也不发送默认模型。
4. 成功后刷新 API 和 Agent 卡片。仅显示刷新失败不能被解释为启动失败，不自动重启或重试。

## 来源与异步边界

弹窗冻结安全显示字段和 `sourceRevision`。后者必须是后端生成的 `src-v1-` HMAC 标识，不是原始密钥或未加盐密钥指纹。旧宿主没有版本值时，本入口拒绝启动并提示刷新/更新。

三个写接口都带 `expectedSourceRevision`，由配套后端 CAS 再检查，防止最终 GET 与 POST 间同 ID 换密钥的竞态。前端不会把 `keyMasked` 当成完整来源证明；同首尾但中间不同的 Key 也须由后端版本区分。后端实现与 Go 验证由独立任务完成，不以本文件的 UI 模拟替代。

- 单一启动写锁阻止重复点击及跨卡同时启动。
- 弹窗节点、路由、代次和状态所有权阻止迟到发现/模型/状态/启动回复覆盖新弹窗。
- 确认后的 ID、工具、覆盖选项和来源版本冻结，不从后来点开的卡片读取。
- 模型刷新中发现账号被删除或来源改变，会取消确认；Claude 空账号不会再触发渲染空指针。
- 旧 Agent 的共享 bind / model-load / open 与 Claude load / go 路径加入必要的所有权保护，避免晚到旧请求污染新 API 卡片入口。
- mutation 失败或结果未知后撤销当前确认，重新打开后才能再次操作，不后台盲重试。

## 验证

### 后端来源确认合同

`GET /api/local/accounts` 的每条可调用账号增加 `sourceRevision`。它复用现有 `Config.AccountSourceRevision`：域分离 HMAC-SHA256 的密钥为启动时随机生成、仅驻留内存的 `GatewayRuntimeKey`。不是直接返回 `AccountFingerprint`，不会泄露原始 Key，也不提供无服务端密钥的离线猜测摘要。未启用运行时返回空值；重新启动后同账号的版本也不同。

普通 Key 即使首尾相同、中间被替换，版本仍会改变；地址、认证、接口、有效模型/策略及官方凭据引用或 `CredentialEpoch` 改变也会改变。名称、最近使用时间及候选目录提示不是执行来源本身，不单独改变该标识。

`POST /api/local/bind`、`POST /api/local/switch`、`POST /api/local/claude-desktop/switch` 增加可选 `expectedSourceRevision`。既有调用省略保持兼容，新卡入口始终传入；显式空字符串或来源不匹配拒绝，不能降级为旧流程。bind 在配置事务锁内验证，失败不修改工具选择。switch 在读取模型或关闭应用前验证初始来源，再核验目录刷新后的来源与原进程身份，最终发布启动授权时再次验证。

启动时自己的目录刷新可能合法清除已下架的默认模型，故只允许既有目录事务确认过的这项更新形成内部新快照；不接受等待期间外部更改凭据或运行时密钥。最终验证失败不会启动新程序，仍走既有配置回滚。源码范围：`internal/console/local.go`、`workbench.go`、`claude_desktop_3p.go`、`local_source_revision.go`；未改反代执行、模型调用或其他配置接口。

新增 `local_source_revision_test.go` 与 `local_source_revision_launch_test.go` 共 7 个测试函数，使用本地合成服务/模拟启动器，覆盖不透明 DTO、同脱敏 Key 替换、字段分类、官方身份版本、三写入口初始拒绝、合法目录同步、进程变更、Codex/Claude Desktop 在正常退出等待期间换源导致最终授权拒绝。定向结果：`go test ./internal/console -run TestLocalSourceRevision -count=1 -timeout=180s` 通过（7.124s）。

上述 CAS 补丁完成后的 `go test -race ./internal/console -count=1 -timeout=600s` 完整包通过（66.354s），随后 `go vet ./internal/console`、局部 diff 与新增文件尾空格检查通过。这个 Go 进程启动于另一个 API 客户端 rotate 端点实施之前，不把它冒充后续功能的验证结果。另独立复核并重跑当前快捷启动 VM：20 条通过，包含首次读取新来源时清除对缓存摘要的提前确认。

执行：

```text
node --test scripts/test-account-launch-ui.mjs desktop/test/claude-desktop-ui.test.cjs desktop/test/api-model-editor.test.cjs desktop/test/agent-workbench.test.cjs
```

57 条通过，其中新增 20 条实际脚本 VM 测试。覆盖四工具暂存/确认、官方与网页会话边界、工具发现、空来源版本、同 ID 来源变化、重复/跨卡点击、关闭/导航、模型刷新不预绑定、Claude 专用事务、来源 CAS 拒绝无重试，以及旧模型回复不覆盖新弹窗。另覆盖同工具同 HTML 但新 DOM 的弹窗所有权、Claude 状态回复晚于后来 Codex 弹窗，以及首次读取来源完成后清除缓存摘要上的提前确认。

旧测试仅更新本轮确实改变的两个函数快照，并去掉已移除认证/协议选择框的操作。两份生产 JavaScript 语法检查与局部 diff 检查通过。

所有数据为 synthetic；没有读取真实凭据、运行原生工具、发起付费调用或改动用户配置。此记录不等于真实 Windows 安装包/工具重启验收，也不代替配套后端来源 CAS 测试。
