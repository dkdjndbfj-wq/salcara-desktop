# Codex 原生宿主复核（2026-10-11）

本轮只读检查本机安装包及公开的工具协议形状，不复制 Codex 原生实现，不读取账号、Key、聊天配置，不调用真实原生发送。

## 已研究的新增版本

- Windows x64 包：`OpenAI.Codex_26.1007.2314.0_x64__2p2nqsd0c76g0`。
- `app.asar`：567399024 字节，SHA-256 为 `c5fdef328cfa2ad9095786f9c3a7aaa447e68834d31299d6ef2592da02029038`。
- `Codex.exe` Authenticode 验证为 Valid，签名主体为 OpenAI OpCo, LLC。
- 静态传输仍使用 4 字节小端帧；`tools/list`、`tools/call` 与 `success/contentItems` 的字段契约兼容现有独立适配器。`read_thread` 仍提供 schemaVersion 1、真实 thread ID、newest_first 及 opaque 分页。

`host-policy.mjs` 新增这个明确版本的路径、摘要和独立大小上限。旧两版本仍有原 512 MiB 上限，未知相邻版本仍拒绝，不扫描或自动信任新安装包。本机实际 `verifyKnownHostResources` 返回 true（约 2.1 秒）。目录、祖先、archive 的重定向和符号链接检查仍执行。

组件、manifest、诊断协议及 Bridge 安装器同步为 0.4.2。安装器保留 0.4.0/0.4.1 的 owned hook 卸载兼容；新旧资源不原地覆盖，外部编辑的配置不自动改写。

## 对话投影的真实边界

当前原生 `read_thread` 不提供 CLI 的 `userMessage.clientId`，且省略部分动态工具输出与结构化错误字段。原生发送通过 `send_message_to_thread` 转发，因此手机自己的发送也可能以 `codexDelegation` 回显。手机在收到本次成功回执后只清理精确的 localId；相同正文、委派类别和来源 UUID 都不被用来认证自己的回显。不确定发送保留同一操作回执，旧 owner 结果不能修改新连接。

`codexDelegation` 是真实宿主扩展，但来自对文本 wrapper 的解析，没有签名或发送方认证。工程只保留有界 input 和委派类别，不转发 sourceThreadId 为可控目标或可信来源。

## 仍需验收

当前承接任务的 shell 继承了工具管道，但未提供 `CODEX_ELECTRON_RESOURCES_PATH`。这不是实际 MCP 子进程环境验收；若 companion 启动时同样缺该字段，连接仍拒绝。版本摘要只证明研究过的 archive，不证明当前管道/实际 API 来源。

仍需在实际 MCP 启动环境中验证宿主信号、控制会话授权、目标会话、审批 hook、实际 API 来源及 Bridge → Hub → Android 链路。没有修改用户 Codex 安装配置、开启真实控制或发布发行包。真实来源缺失时保持能力关闭。

## 回归

Node 全量 96/96 通过；覆盖新增 archive 尺寸、摘要替换、旧版本上限、未知版本、原生范围、回执和取消。Bridge 安装器覆盖 0.4.0/0.4.1 安全卸载后安装新版本，保留用户设置和可恢复旧资源。手机覆盖 native 成功/不确定回执、同文本委派、owner 变更、同毫秒两条 CLI 输入及分别 clientId 确认。合成测试不替代真机验收。
