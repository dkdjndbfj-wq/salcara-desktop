# 远程会话管理、审查目标和独立提问 · 2026-10-15

本阶段延续 `REMOTE-CONTEXT-20261015.md`，记录实现和对应验证边界。Claude 仅考虑 API 接入；SIWC 按用户要求暂缓。本阶段完成不等于全部 Cockpit/Codex 产品功能和真实账号链路已经验收。

## 已接入的操作

| 范围 | 行为 | 能力边界 |
| --- | --- | --- |
| Codex 原生会话管理 | 原生改名、归档/恢复、归档列表、分支；手机别名仍单独存在 | 实际 initialize userAgent 必须为已审计的 0.145.x；历史/只读/桌面控制面不假装 CLI 操作 |
| Codex 审查 | 未提交更改、基准分支、指定提交、自定义要求四种目标，固定 inline | 只有匹配 thread 和 turn 的接受回执才显示开始；不等于审查已完成 |
| Codex Goal | 读取、设置、暂停、继续、清除；展示六类原生状态和真实 token 用量 | 未指定预算不代填；active Goal 在轮次间隙保留调度占用；暂停不冒充中断当前轮次 |
| Claude 原生会话管理 | 官方 standalone SDK rename/fork，原生标题读取和分支归属 | 正式 SDK 0.3.300 必须通过版本、导出、Node/模块文件身份与内容校验；缺失时明确不可用 |
| Codex / Claude 独立侧边提问 | 以主对话已加载内容的冻结快照开启临时问答，保留已确认的追问链 | 只走当前已绑定账号的本机网关，固定模型，无工具；不恢复/继续父会话，不改父消息或草稿 |
| 手机界面 | 紧凑的会话管理、归档、审查与目标面板；独立提问 UI 由协作会话整合 | 页面关闭、换源、换会话、失去前台及迟到响应均需重新验证 scope |

Claude 归档、专用 Review/Goal 仍按其真实运行时合同保留不可用，不能把 Codex 同名能力直接套用。安装版 CLI 2.1.220 缺可靠 builtin 来源标记，不能试发 slash 猜支持，避免被自定义同名命令接管或退化为付费普通消息。本机正式管理 SDK 尚未安装；桌面设置已有固定版本的可选组件安装项。

## 关键隔离与并发处理

- 所有新操作都验证精确会话、CLI 控制面、API 账号、中转站来源、授权代次、运行时身份和 revision CAS。A→B→A 不复活旧授权；未知原生结果保留 operationId 回执，避免重复分支、压缩、审查或消费请求。
- 迟到的 Codex 管理回执使用有界 observer（最多 256 项），不额外开无限 goroutine，不用闲置快照猜操作成功，也不通过杀共享 worker 恢复状态。真实晚到归档确认先投影归档状态再解除占用。
- Claude SDK helper 用 ready/commit 握手，最终提交前重查源和文件内容；fork 只有原生父会话元数据、结果文件身份和来源均确认后才授予 Bridge 归属。长历史标题逆向分块读取有界，超界显示待确认，不退回旧标题冒充最新值。
- Claude context 修复手机选 B、电脑选 A 时的合法来源误拒。原请求 guard 仍响应取消；原生操作完成后的生命周期 guard 仅去除 HTTP 取消，继续验证手机绑定、站点/账号代次、权限与 worker，防止正常 HTTP 结束被误当成来源撤销。
- 独立提问只接受 Bridge 从 `session.open` 得到的主会话 user/assistant 快照；不接受客户端伪造上下文或 assistant 历史。最多 80 条/64 KiB 快照、6 轮、20 分钟，完整输入预算 96 KiB，答复 16 KiB，HTTP 40 秒。每次打开面板有独立 revision，互不替换。
- 侧问进行中阻止 API 切换，但不占父任务 Send/Steer/Interrupt 的锁。父消息继续更新不修改已冻结快照；父上下文不足或截断不能被包装为完整上下文。
- Codex 请求使用 `tools:[]`、`tool_choice:"none"`、`store:false`，Claude 使用无工具 Messages 请求。固定本机 HTTP 路径，不接受环境代理、重定向或任意上游 URL。API-key Responses 保留输出 token 限额，官方 Codex 不传其不支持的字段，仍保留时间和响应体边界。
- 已知容量、401/403、429、HTTP 状态、连接失败等只返回固定错误文案；不透传上游正文、凭据、URL。网络请求已尝试时保持结果未确认，不自动重发；相同 operationId 返回原回执。
- Hub 七类状态读取（context/models/features/actions/archives/workflows/side catalog）不持久占用 24 小时回执，取消时释放挑战与 pending。compact/action/workflow/side ask 仍使用持久幂等回执，手机取消不等于取消已提交的原生操作。

## 验证记录

全部 provider 调用使用 httptest、本机合成网关或合成 SDK 上游；没有真实 OAuth、付费模型调用、生产部署或修改真实会话。

| 检查 | 已取得结果 |
| --- | --- |
| Codex 管理 | 普通 15.597s；扩大 race 38.327s；最后迟到 observer 管理/RPC race 22.482s；vet 通过 |
| Codex 审查/Goal 与管理 | 普通 40.520s；扩大 race 80.539s；agents/protocol vet 通过 |
| Claude 管理/标题/shared side | 管理扩大 race 20.260s；shared+Claude side+actions/title race 13.327s |
| Bridge 新命令公共调度 | actions/workflows/side 合并 race 6.200s 通过 |
| 最终运行时替换与手机撤销 | 所有新 dispatcher（context/actions/workflows/side）合并 race 17.050s；包含同配置新 adapter 替换、移除与响应前后校验 |
| Claude 生命周期最终合并 | 普通 agents 21.241s / hubclient 5.352s；race agents 22.934s / hubclient 7.727s；两包 vet 通过。真实非空 binding/hash、上传授权、device、权限和来源代次撤销均覆盖 |
| Bridge 冻结后整树 | `go test ./...` 自然完成，所有包通过；agents 487.040s、console 123.113s、hubclient 87.565s。部分未修改小包使用有效 Go 缓存，未表述为全部无缓存复跑 |
| Bridge 静态/构建与快照 | 全仓 `go vet ./...`、Windows amd64 构建均 exit 0；747 Go/mod/sum 文件及 81 资源/脚本文件验证前后 SHA 相同 |
| 独立提问错误分类与两 provider | shared+Claude/Codex side 全组 race 2.804s 通过；覆盖拒绝重放、原始错误脱敏和 8 类 HTTP fixture |
| Codex 实际 SDK 重写边界 | 最终侧问普通全组 2.316s 通过；实际 pinned SDK 对合成上游发送空工具和 none，官方 buffered completed wrapper、Chat/Messages 转换均通过 |
| Hub 七类读取与四类 mutation | 最终 race 66.803s 通过；每类连续 258 次读取；撤销、nonce、取消、旧缓存均覆盖 |
| Hub 整树 | 协作会话 go test/vet/build 均 exit 0；验证前后源文件哈希相同，结果文件见下方 |
| 手机定向 | 管理 36 项、workflow 47 项、side parser/transport 16 项通过；一次高负载 UI 5s 超时后该套隔离复跑 9/9 通过 |
| 手机最终整树 | 含首次技能 TextWatcher 与键盘修复后，tsc 通过；147 suites、2467 tests 通过，8 个既有 obsolete tests skip；415 文件验证前后 SHA 相同 |
| 浏览器实际组件 | 管理、审查/Goal 在 320px 浅深色无横向溢出；Goal 预算 0 禁用提交，合法预算恢复；0 JS error、3 条共享 RN Web/AppDialog warnings |
| Android 原生与正常入口 | 合成组件的长按/双手柄、重复文字及跨段 emoji 精确回源、注释、独立侧问/追问、键盘与 1.3 倍字体通过；正常 APK 安装后 COLD / Status ok，启动 2.737s，欢迎/配置/添加 API 导航通过 |
| 最后只读交叉复核 | 独立子代理复核侧问固定错误分类/no-replay 与 actions/workflows adapter 身份校验，未发现有证据的缺陷；未修改已冻结源码 |

第一次 Hub race 的长循环跨过测试初始 grant 的 60 秒期限而失败；测试 helper 现每轮签发新有效期，与实际来源签发流程一致。未放宽生产授权期限。手机第一次全量发现长按复制与新选区行为冲突，协作会话恢复显式“复制消息”后全量通过，未删除实际复制断言掩盖回归。

本方 Bridge 源码已冻结，协作会话完成全仓 test/vet/build。Android 实测发现的侧问键盘遮挡、首次技能标记未生效均已修复，并在模拟器合成环境的冷启动、键盘弹出及原生按钮触发发送回调中复测通过；修复后手机全量结果见上表。正常 App 测试 APK 已排除 fixture 专有内容，并通过安装、冷启动和主要导航检查；应用 PID 日志无 FATAL EXCEPTION、AndroidRuntime 或 UnsatisfiedLinkError。未输入凭据或请求模型，本方独立复算 APK SHA-256 与清单一致。

应用使用用户当前系统输入法，未捆绑 Gboard。正常入口之外的 474 个打包输入与原生夹具一致。sherpa 离线语音库仅有 arm64-v8a，因此 x86_64 模拟器通过不证明离线语音可用。实体真机、其他厂商输入法、中文组合输入专项、iOS、真实账号、弱网休眠、签名正式发行与部署验收不包含在本轮本地结果内。

本地测试组件已构建，版本标识 `acceptance-20261015`，未安装服务、未部署、未正式发布。以下均位于协作会话 `work/acceptance-20261015/`：

| 测试产物 | 大小 | SHA-256 |
| --- | --- | --- |
| `android-package-final/salcara-remote-test-acceptance-20261015.apk` | 156,694,517 bytes | `520f10218dff7568e1778759786c71a4c5134828d4ea707a08bc8682e3cc7284` |
| `bridge-final/salcara-bridge-windows-amd64-acceptance.exe` | 75,343,360 bytes | `a1183f1c0af611df0d5445ec3e7be25150c219b8c34edc8dc92b0d4928d08f94` |
| `hub-final/salcara-hub-linux-amd64-acceptance` | 12,701,802 bytes | `d92e96ff63204ad9f041033830b0214d06cee8c2e6381bc2aede2ee963e7384b` |

## 全项目仍需保留的边界

本阶段不能据此回答“与 Cockpit 全部对齐”。以下项目在前序功能复核中仍明确保留，未被本次会话管理功能隐式完成：

- 原生 Codex App / Claude Desktop 任意外部启动进程的实际 provider 来源证明与完整写控制。生产来源保护启用时仍拒绝无法证明来源的接管；受管理 CLI 可用不等于原生应用控制已开放。
- API 服务专属的全局 `auth.json` / `config.toml` 备份、显式应用、外改检测、停止/退出恢复已在后续批次补齐，详见 [服务配置接管](CODEX-SERVICE-PROFILE-20261015.md)。它与 Workbench scoped apply 独立互斥，不等于原生进程来源证明；上方早一阶段构建和测试记录不包含该新增功能。
- 真实第三方站点 issuer 的实现与部署，以及真实账号 OAuth/额度/套餐/重置消费的目标环境验收。本地 synthetic 连接器合同不意味着每个已保存站点都已支持；手机仍按实际探测结果开放能力。
- Claude 专用原生 Review/Goal 和归档保持上述版本/合同边界；SIWC 按用户要求暂缓。第三方软件共用同一 refresh token 的跨进程仲裁也按用户后续指示不继续扩展。

后续文档中的旧日期“未接线”条目应与本阶段及协作会话的新证据交叉核对；上述实际限制不能靠改文档勾选为已完成。

## 证据位置

- 本会话 `output/NATIVE-SESSION-SUPPORT-CODEX-20261015.md`、`output/CLAUDE-API-NATIVE-SESSION-OPERATIONS-AUDIT-20261015.md`。
- 本会话 `output/playwright/session-actions-20261015/`、`output/playwright/session-workflows-20261015/`。
- 协作会话 `C:/Users/24230/Documents/Codex/2026-10-14/codex-threads-01a0ae56-e9f6-7933-9ad4/work/acceptance-20261015/hub-final/` 和 `mobile-final/` 的 result/source-verification 文件。
- 本轮完整交付报告和三端产物清单位于协作会话 `outputs/ACCEPTANCE-REPORT-20261015.md`、`outputs/ACCEPTANCE-ARTIFACTS-20261015.json`；原生检查位于 `work/acceptance-20261015/android-fixture-ime-fix/NATIVE-FINAL-RESULTS.md`，正常包安装记录位于 `android-package-final/`。APK 为独立包名 `top.salcara.image.remotetest` 的 1.8.2 / 23，Release 构建、开发测试签名，不是正式发行包。
- 本会话 `output/remote-session-workflows-20261015.source-sha256.json` 记录本方冻结的 57 个 Bridge 源码/测试/直接接线文件（包含 Claude context source/admission 补充测试）；完整仓库仍以协作会话整树快照为准，不能把旧时间点的全仓通过视为后续共享树修改也通过。
