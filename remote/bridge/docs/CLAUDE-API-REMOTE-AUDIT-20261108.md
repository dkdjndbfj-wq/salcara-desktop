# Claude API 与远程链路修复验收（2026-11-08）

范围：Claude Code 的 API 配置、本地启动、Direct、Hub、手机能力入口和会话控制。账号登录不在本次范围内。Claude Desktop Code 的受支持能力来自已验证的 Claude worker；Chat/Cowork 历史仍按现有适配器提供只读能力。

## 已修复

| 问题 | 修复结果 |
| --- | --- |
| 旧环境变量或用户/项目 settings 覆盖所选 API | 本地启动和远程 worker 使用私有 `--settings` 固定 API；清除竞争的云路由、OAuth 环境和自定义鉴权头，保留无关设置及项目文件。 |
| 新版 npm 安装的 Claude 入口无法正确启动 | Windows `.cmd/.bat` 入口解析到包内原生 `bin/claude.exe`，继续兼容旧 JS 入口。 |
| 原生 CLI 的 `messages?beta=true` 被网关拒绝 | 只允许 Claude messages/count_tokens 的明确 beta 查询；Claude→Anthropic 保留 beta 参数和头，跨协议不透传。 |
| Desktop Code 能力投影遗漏 | 复制已校验 worker 的 features、context、annotations、side question、compact 和 session actions 等能力；不可写 worker 不扩展为可执行。 |
| Direct 缺 API 仍进入创建/发送流程 | 手机创建、发送和待发恢复均使用统一 API 就绪判断，保留草稿并引导选择 API。 |
| 只安装 Claude 时被缺少 Codex 阻断 | 工作区准入仅跳过明确的 Codex 未安装错误；其他探测故障和活动占用仍拒绝。 |
| Direct 完成后工作区未释放，快速完成又被启动回执标回忙碌 | Direct 事件先更新本地工作区占用；启动观察器防止晚到回执覆盖已收到的终态。 |
| Windows 启动回执丢失时提前删除仍在使用的 API 配置 | 已启动或启动状态不确定时保留配置，等待所属进程清理；界面明确显示状态尚未确认。 |
| Claude 停止后重新打开会话丢失终态编号 | 在已确认退出且原历史未改变的条件下，历史快照合并最后终态及版本号；外部修改或删除历史会撤销控制资格。 |

## 验证

- `agents`、`launcher`、`toolcfg`、`hubclient`、`console`、`gateway` 六包 Claude/Direct/工作区相关测试已通过 race 检查。筛选共匹配 427 个顶层测试，显式启用的原生测试默认跳过，另行验收。
- 新增停止后历史恢复回归，先复现失败，再验证修复；覆盖正常退出凭据、外部改写、历史删除，并检查 Open、OpenPage、OpenMessagesPage、Sessions、SessionsPage 五个入口。连同退出保护、继续操作、历史所有权和快照版本相关 race 回归通过。
- 实际安装的 Claude Code 2.1.220：通过 TLS Direct 配对、手机 API 创建/选择、原生执行、两轮历史、手机审批后原生 Write、同工作区新建任务。
- 实际 Claude → Bridge → Hub → 模拟配对手机：修复后连续 4 轮完整通过真实 QR 配对、API 选择、启动/续聊/历史、手机审批 Write、精确停止及上游取消、重连终态恢复、同会话 Continue、同 operationId 重放无额外推理、同工作区新建任务。每轮 7 次合成模型请求。
- 本地启动 API 优先级：实际 Claude 在环境、用户设置、项目设置、本地项目设置均有冲突配置时仍使用所选 API，原配置文件保持原样。
- 手机 5 个针对性测试套件、56 个测试通过，TypeScript 检查通过。浏览器在手机与桌面尺寸验证缺 API 引导和配置后创建入口。
- Linux 启动器交叉编译通过；没有做 Unix 实机执行验收。

真实 CLI 验证使用隔离 HOME、合成凭据和本机模拟 API，没有调用付费模型。手机端到端命令通过模拟配对手机发出，没有声称实体手机 APK 或真实相机验收。

## 当前能力与验收边界

- 手机连接断开后重连、同一个 Bridge 进程内停止后继续，已通过上述端到端验收。Bridge 自身重启后的旧会话自动接管不在已完成能力中：`claudeHistory.ownedLeases` 和 `claudeAgent.live` 只保存在当前进程内，重启后仍可读取历史，但不能仅凭历史文件认定原进程已停止并继续执行。
- Claude Desktop Code 的可执行能力来自 Bridge 托管的 Claude worker；Chat/Cowork 仍为只读历史，不能据此宣称已完整远程控制原生 Desktop。
- 实体手机 APK、公网连接、真实 API 提供商及 Unix 实机没有完成这轮端到端验收；本机模拟 API 验收不能替代这些检查。
- 源码和测试构建已完成，尚未替换安装版或发布。“已修复”指本次识别的缺陷，不能等同于全部产品场景均已覆盖。

## 可复跑入口

- `internal/console/claude_native_direct_e2e_test.go`：设置 `SALCARA_CLAUDE_E2E_EXE`、`SALCARA_CLAUDE_E2E_BRIDGE` 后运行 `TestClaudeNativeDirectAPIExecution`。
- `internal/launcher/claude_native_api_e2e_test.go`：设置 `SALCARA_CLAUDE_E2E_EXE` 后运行 `TestClaudeNativeLauncherAPIPinPrecedence`。
- `tools/claude-native-hub-smoke.cjs`：四个参数依次为 Claude 原生程序、output 目录下的 Bridge 测试构建、Hub 测试构建、已有 Hub 临时目录，均为绝对路径。使用独立的 47861/47862 端口。

Hub 测试目录所在卷须满足生产存储预留要求；C 盘空间不足会正确返回 503，应改用有足够空间的测试卷，不放宽生产保护。Windows 上 Go 测试编译可使用 E 盘 `GOTMPDIR`，运行测试程序前清除该变量，以便私有配置 ACL 测试使用系统临时目录。

测试构建位于 `output/fixture-bridge-claude-audit.exe`（78,746,624 字节，SHA256 `551A12B4D29A6A91A31E62265BCDEFC6426C86B898C3A2E8ABDD5180BBE20E8A`）。最后一轮 Hub 结果保存在 `output/claude-native-hub-smoke-524PKG/acceptance-report.json`。本次不包含替换安装版或发布。
