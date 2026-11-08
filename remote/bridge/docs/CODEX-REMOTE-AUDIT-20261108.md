# Codex 远程链路核查（2026-11-08）

本次核查以当前源码和已安装 Codex app-server 为准，不把旧版 parity 表格中的缺项直接视为现状。手机 Hub、手机 TLS Direct、原生 Codex Desktop 控制分别核查。

## 确认的问题

| 链路 | 问题 | 修复方向 |
| --- | --- | --- |
| 直连停止 → 网络错误 → 自动恢复 | 停止的不确定结果错误地写入消息发送状态，读到终态后仍禁用发送与停止 | 单独保存停止目标轮次；仅匹配该轮终态或有效回执才解除等待 |
| 直连审批和用户问题 | 页面未按 Agent 的 approval/questions 能力禁用操作 | 页面与发送处理函数同时检查能力，保留会话和 API 来源 |
| 直连问题草稿 → 断线重连 | 问题卡卸载后丢失未提交选项和自由文本 | 按配对、会话、请求身份持久化草稿 |
| session.continue → 工作目录检查 | 未复用普通发送的工作目录冲突检查 | 在继续任务前执行同一目录冲突检查 |
| 原生审批策略 → 续接已有会话 | 新版返回 granular 对象，旧解析器仅支持字符串 | 兼容对象解析，保留原策略，不将其误报为更宽的 UI 权限模式 |
| Hub API 快照 → 元数据读取 → 后台历史刷新及手机分页列表 | 后台读取按默认来源重启连接，使远程请求收到 runtime_connection_changed；分页刷新后已完成的手机任务被误判为电脑正在运行 | 普通列表、分页列表、元数据和历史读取复用活动连接；执行与携带来源快照的读取仍校验来源 |
| 手机选择 API → 恢复原 Codex 会话 | Hub 强制改为 salcara provider，原 openai 会话被新版 Codex 拒绝恢复 | 保留已应用 provider，无已应用标识时使用 openai；地址与密钥仍来自明确选择的 API |
| 手机权限模式 → 原生确认 | 当前 0.167 对已加载会话忽略旧 resume 覆盖参数，新建会话的旧 sandbox 参数也未形成所选权限 | 不匹配时使用 thread/settings/update，已有会话通过 resume 回读有效权限元组 |
| 手机新建任务 → 首条输入 | 首条输入之前没有 rollout，使用 resume 核验权限会失败 | 新建会话在更新前订阅 thread/settings/updated，只接受同连接、同会话且匹配的原生有效权限；空回执、错误身份、未确认权限均不发送输入 |
| 重试回执 → API 来源指纹 | 手机传来的不透明 API handle 未解析，回执错误地绑定电脑默认 API | 先解析 handle；选中 API 的凭据改变后拒绝旧回执，无关电脑 API 变更不影响它 |

协作的 Claude 核查另修复了“未安装 Codex 时阻断 Claude 工作目录检查”。只跳过确定未安装的错误，其他检查故障仍拒绝执行。

## 当前能力边界

- 配对和执行 API 来源分别验证；Hub 在线、能读取会话历史均不代表已经获得执行授权。
- Bridge 可以使用受管理的 API 继续已有 Codex 会话。会话 ID 与历史需要在实际端到端验收中保持一致。
- 原生 Codex Desktop 的窗口控制是独立通道。生产 RemoteExecutionArmed 状态下，因为适配器尚不能证明桌面进程的实际 API 来源，原生发送、审批、模型覆盖和导航仍关闭。保留此边界，不通过删除校验或改走 CLI 冒充原生控制。
- Direct 不发布原生 Desktop 控制能力。
- 同一安装里混合了不同 provider 的历史仍受 Codex 原生限制：不能用一个 provider 恢复另一个 provider 的会话。本次不静默分叉、复制历史或退回未受管理的旧凭据。
- 本次不覆盖真机安装、APK 发布、真实供应商计费或任意外网环境。

## 可复跑验收

`tools/native-session-smoke.cjs` 接受四个绝对路径：已安装的 Codex 可执行文件、`output/protocol-smoke.exe`、隔离 Bridge 二进制、隔离 Hub fixture 二进制。所有 HOME、配置、凭据和上游均为本机测试数据。

验收依次检查：原生 Codex 创建并保存会话；退出原生产者并替换其旧测试凭据；Bridge 读取同一会话；实际二维码 PNG 配对；手机选择 API；通过 Hub 继续同一会话；手机批准测试工具；用户消息、回复和终态经 SSE 回传；精确停止后确认上游取消；重连后读取原历史和终态；停止后继续任务；同操作重试不产生额外模型请求；原生认证文件未被手机执行改写。

已完成真实 Codex 0.167.0-alpha.3.2 的两条本机验收；手机端由协议客户端模拟，没有使用真机摄像头或付费模型。上游只接受测试密钥及 non-openai-fixture 模型。

| 验证 | 结果 |
| --- | --- |
| 原生 app-server → 独立 Bridge → 实际 Hub → 配对手机协议客户端 | 原会话与历史保留、分页刷新后继续、手机批准 exec_command、流式回复和终态、精确停止与上游取消、断线重连、停止后继续、同操作回执重放均通过；5 次模拟模型请求 |
| TLS Direct → 生产控制台/网关/Agent → 实际 app-server | 新建 ask 会话、API 选择、分页刷新、审批、精确停止、重连、继续、回执重放及原认证文件不变均通过；race 模式，9.01 秒，5 次模拟模型请求 |
| 手机恢复与控制 UI | direct-control-recovery、direct-control-gates、direct-question-drafts：3 套、27 项通过；TypeScript 检查通过 |
| Go 定向回归 | 权限模式、新版设置通知、后台/分页读取、API provider、来源约束、继续任务工作目录与回执来源的 race 测试通过；未将其表述为仓库全量测试 |

Hub 最终完整成功报告：`../output/native-session-smoke-GL69k1/acceptance-report.json`，包含继续操作的新轮次 ID，并验证重连时的旧终态不被当作新轮次完成。直连可复跑测试：`../internal/console/codex_native_direct_e2e_test.go`，设置 `SALCARA_CODEX_E2E_EXE` 和 `SALCARA_CODEX_E2E_GATEWAY` 后运行 `TestCodexNativeDirectAPIExecution`。

验收 Bridge：`../output/codex-audit-bridge.exe`，版本 `Salcara Bridge dev`，SHA256 `551a12b4d29a6a91a31e62265bcdefc6426c86b898c3a2e8abdd5180bbe20e8a`。这是独立验收产物，未替换用户安装、未发布 APK。

环境说明：初次 Hub 事件上传 503 来自测试 C 盘空闲空间低于持久存储保护阈值 `max(卷容量/20, 2 GiB)`。保留生产保护；验收脚本支持 `SALCARA_NATIVE_SESSION_HUB_FIXTURE_ROOT`，本次将新建的隔离 Hub 数据写入 E 盘。未以删除用户文件或降低阈值解决。
