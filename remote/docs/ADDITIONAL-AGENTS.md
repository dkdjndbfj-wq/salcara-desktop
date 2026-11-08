# DeepSeek Harness 与 Hermes 接入

更新：2026-11-08。此文说明当前源码实现和验收边界，不表示已发布新安装包。

## 使用

1. 在电脑的 API 密钥库保存服务商地址、密钥和上游协议。没有模型发现接口时，可在高级设置填写模型目录。
2. 在 Agent 页安装或指定 DeepSeek Harness / Hermes 的官方程序路径。
3. 选择 API，确认已结束当前任务，再选择“仅应用配置”或打开 Agent。配置写入前备份，失败回滚；仅应用不启动或关闭进程。
4. 手机通过已配对的直连或 Hub 选择对应 Agent 和 API，再在项目中新建对话。直连支持在电脑保存新 API 和安装白名单中的 Agent。

写入原生工具的密钥是 Bridge 本地网关凭据，上游密钥保留在电脑的 API 库中。Bridge 必须保持运行；重启后应重新应用原生工具配置。切换 API 的成功状态表示配置已应用，不代表已完成真实模型推理。

## 适配范围

| 能力 | DeepSeek Harness | Hermes |
| --- | --- | --- |
| 原生打开 | `dsh web` | `hermes chat` |
| 手机执行 | 官方 SDK JSON-RPC | 官方同用户 Gateway，通过安装版 Python 客户端接入 |
| 新建与发送 | 支持 | 支持 |
| 模型选择 | 新建和后续轮次 | 仅新建；会话的模型和 API 固定 |
| 历史 | Bridge 创建的会话 | Bridge 创建的会话 |
| 审批 | 原生一次审批 | 仅原生已声明的 once/session/deny |
| 停止 | 当前轮次核对后结束该会话独占运行时；活跃子 Agent 时拒绝 | 原生 execution generation 校验；等待终态才报告已停止 |
| 图片、附件、结构化提问、子 Agent 管理 | 尚未开放 | 尚未开放 |

Hermes 原生 Gateway 冻结会话的 API key 和 base URL。Bridge 凭据轮换后，旧会话不能通过传入新凭据恢复发送；应新建会话，旧记录仍可读取。关闭 Bridge 只会断开 Hermes 查看端，不会结束共享 Gateway 中已经接受的后台任务。详见 [Hermes Gateway 协议说明](HERMES-GATEWAY.md)。

## 安装与配置

- DeepSeek Harness 安装入口固定为本轮验证的 `@deepseek-ai/dsh@0.2.2-rc.1`，要求 Node.js ≥24.9；目前属于开发预览版本。配置位于 `DSH_HOME` 或 `~/.dsh` 的 Web profile 与凭据文件。主目录存在覆盖模型或凭据的高优先级 patch 时拒绝应用，防止显示成功但实际使用旧 API。
- Hermes 使用官方安装器。Windows 远程适配要求官方原生 `hermes.exe` launcher；不通过猜测 `.cmd` 里的 Python 环境接入。默认使用 `%LOCALAPPDATA%/hermes`，可通过 `HERMES_HOME` 指定。其 `config.yaml` 仅修改 model 中所需的连接字段。
- 两款 Agent 的 local、Direct、Hub 路由与授权独立。Claude Code / Desktop 的共享 API family 不会把凭据串给新 Agent。
- 原生 YAML 的其他配置保留；格式错误或重复键会拒绝覆盖。

## 验证记录

- 官方 DeepSeek SDK，隔离安装与本机合成上游：创建、续聊、改模型、跨 Bridge 恢复、活动请求精确停止通过；官方 Web 配置组合验证通过。没有调用付费模型。
- 官方 Hermes 源码 `b52b8b00f0c2af6b6d6ede68d9071be8cf2d1b5a` 与锁文件核心运行时（隔离 Python 3.14.6），生产 sidecar 接本机合成 OpenAI 服务：创建、两轮回复、适配器重建后使用原凭据续聊、官方拒绝不同凭据、精确停止到原生取消终态通过。该验证不经过官方安装版原生 launcher，也不使用付费模型。
- agents 包完整 `go test -short` 一次通过（540.992 秒）；两款 adapter 定向 race 通过。Hermes 最后11项专项及凭据轮换审批专项 race 通过，Python sidecar 四项和官方凭据重绑源码契约一项通过。
- Hermes 官方运行验收发现的历史/重放重复已按 native persisted row ID 与提交回执修复；官方 `gateway_input_owner` 是主体标识，不能当作任务 admission，使用 `submission_id` 与已接受任务关联。最终六类 history/诊断 race 及官方完整 `-race` 验收通过（52.02 秒）：两轮相同文字对应两条消息、恰好三次模型请求，第三次被精确取消。
- 手机新建模型能力：三个套件、82 项通过，包含创建后不能继续切换模型及不能伪装支持推理档位。
- 桌面 API 编辑、六张 Agent 卡片、仅应用与启动流程：本轮32项相关 Node 测试通过。1120 和390宽度浏览器交互通过，无横向溢出。
- Direct 真实 TLS 配对、保存 API、选择 API、worker 配置、production Gateway 请求、清除与撤销旧凭据已通过隔离合成上游验证。
- 协作验收还覆盖真实 Codex CLI 对合成 Chat / Anthropic 上游的两轮执行和工具调用；手机管理12项测试与 Android 资源导出通过。
- 新增配置、原生文件、launcher、Gateway、console、能力发布六个包的定向 race 通过；Bridge QA 可执行文件构建通过。
- 跨 Hub 原始 `tools/single-scan-e2e.cjs` 在修复配对旧连接与迟到连接响应后，全8阶段通过（新 profile 三轮）；最终报告为 `bridge/output/single-scan-e2e/run-mi96wm/acceptance-report.json`，一次扫码、零模型请求。覆盖离线恢复、站点切换、token 隔离、重启和撤销，fixture 进程已退出。

Hermes 官方安装器与原生 launcher 的完整实装验收、真实供应商推理、Android APK 与真机跨设备验收仍需分别记录；资源导出不能替代 APK 安装验收。跨 Hub 配对回归不发模型请求，不能替代真实供应商推理验收。

Hermes 中间一次验收出现约130秒后的读取失败，当时 fixture 没保留底层错误与真实 Gateway 日志，原始原因无法追回。补齐日志并修正身份字段后，最终在相同100ms快照读取压力下通过；不能据此断言旧失败是负载或证明其根因已修复。生产端保留安全诊断，读取失败不会自动重发任务。

共享工作区并行改动补齐后，2026-11-08 最后一次全仓 `npx tsc --noEmit --pretty false` 已通过；这仍不代表手机发布安装包已构建或通过真机验收。

最终统一 Bridge 构建：`bridge/output/fixture-bridge-collab.exe`，78,496,256字节，版本命令返回 `Salcara Bridge dev`；SHA-256 为 `52AAB53629AEE685833ABBC95120E95A9B1DCEF0CC60334D95D33E346116BC0C`。仅验证版本并保存 QA 产物，没有替换正在使用的安装，也没有发布安装包。

## cc-switch 参考

参考 [farion1231/cc-switch](https://github.com/farion1231/cc-switch) 的服务商预设、显式协议与认证、手工模型目录，以及配置和打开操作分离。对照提交为 `66e4362b60cf0d93d9776cfe549c2b625e1c6b79`（MIT）。本轮按现有 Go/React 架构实现，没有移植整段源码。连通检测与模型目录读取都不视为推理验证。
