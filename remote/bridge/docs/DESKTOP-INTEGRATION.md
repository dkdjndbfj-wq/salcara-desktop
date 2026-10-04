# 桌面接入边界与源码更新

> 历史技术资料，仅用于维护兼容实现；其中旧插件安装、授权和测试流程不是当前用户操作方法。普通用户请看 [最新使用教程](../../../docs/USER-GUIDE.zh.md)，不要按本页安装旧实验组件。

2026-09-30，Windows x64。本文描述当前源码，不是生产上线或原生桌面端到端验收报告。

v1.4.0：新增 companion v0.2 的活动授权实验通道。原生发送不再永久关闭，但只有用户在原 Codex 明确批准 salcara_desktop_connect 且本机验证成功才开放；最多 20 个原会话 / 10 分钟，不含控制对话。完整步骤与按需网络边界见 [TRIAL-v1.4.0.md](TRIAL-v1.4.0.md)。以下旧只读 probe 说明不能代替 connect 授权。

## 已实现与未实现

| 能力 | Codex CLI / Claude Code | Codex Desktop | Claude Desktop Chat / Cowork |
|---|---|---|---|
| 读取本地编程会话、CLI 续聊与事件回传 | 保留原实现 | 可以读共享持久记录，不等于接管桌面 | 普通聊天库未接入 |
| 请求在原生桌面打开原会话 | 不替代 CLI 执行器 | 已有深链接适配源码，未实机验收 | 未实现 |
| 手机向原桌面会话发送、读取原回答 | 不属于 CLI 控制 | 活动授权实验适配已实现，未真实端到端验收 | 未实现 |
| 自动应用 API | 保留 | 保留已有配置流程，不能当作桌面远控 | 暂停旧配置写入，明确拒绝 |

桌面与 CLI 能力独立显示。Hub 在线、安装程序、应用 API 不等于原生控制；只有验证后的 Codex 活动授权可开放桌面灯。Claude Desktop 仍未接入。

## Codex 桌面会话定位

配对手机发送：

```json
{"type":"desktop.navigate","sessionKey":"codex:0199aaa1-1234-5678-9abc-0123456789ab","controlSurface":"desktop"}
```

Bridge 只接受标准小写 UUID；拒绝 `new`、任意 URL、查询、路径、换行和命令文本。先读取会话并核对完整 sessionKey，然后检查桌面安装情况，再调用系统已注册的固定 `codex://threads/<uuid>` 链接。该命令不调用 CLI Start/Send/Resume，不发送 prompt，不重新配置或重启桌面。

返回仅表示已请求系统处理链接：

```json
{"requested":true,"action":"navigate","controlSurface":"desktop","sessionKey":"codex:0199aaa1-1234-5678-9abc-0123456789ab"}
```

不能据此宣称窗口已显示、手机消息已送达原桌面或回答已回传。插件仅转发命令，仍按设备配对权限隔离，不需要模型 API Key。

官方深链接支持原聊天定位；新聊天 prompt 参数只是输入框预填，并不自动发送。[官方 Commands](https://learn.chatgpt.com/docs/reference/commands)

## 执行器与历史来源分离

SessionInfo 的 controlSurface 区分执行器。CLI 列表/打开为 cli；desktop.sessions.list / desktop.session.open 只使用原生工具，活动范围内标记 desktop 并带 controlExpiresAt，不改变历史来源。身份不一致或越权拒绝。

缺字段、未知能力或未授权只读。desktop 续聊要求当前 Agent 有效授权、同一原会话及范围；停止/审批仅 CLI。旧电脑端须升级。

Bridge 只接受固定 desktop.status / desktop.sessions.list / desktop.session.open / desktop.session.send 等白名单；发送有标准 operationId，回执不等于回答完成。未知命令拒绝，不回退 CLI；旧 CLI 兼容保留。

## Claude Desktop 配置保护

本机 Claude Desktop 2.16120.0 的原生 3P 配置与旧实现不兼容：3P 使用独立数据目录，配置库 ID 有 UUID 契约，模式配置文件也不同。旧 fixture 验证只是写文件成功，不能证明原生应用接受。

Prepare / Launch / PrepareSwitch 在任何发现、备份、配置写入、Stop / Start 前拒绝；低层写入函数同样拒绝，避免绕过。未执行真实用户配置迁移、Import 或模式切换。

未来推理接入建议：用户先在原应用的既有 3P 模式手动配置固定本机网关，再只在 Bridge 内部切换上游 Key；保持数据目录、部署身份和原配置 ID。官方 credential helper 可以刷新推理凭据，但不是桌面发送接口。此建议尚未实现自动配置，不构成现在可用的承诺。

官方明确改变 deploymentOrganizationUuid 会令旧命名空间数据不可见。[配置参考](https://claude.com/docs/third-party/claude-desktop/configuration)、[凭据 Helper](https://claude.com/docs/third-party/claude-desktop/credential-helper)、[数据目录](https://claude.com/docs/third-party/claude-desktop/data-storage)

## 完整桌面续聊仍需验证

当前未找到公开稳定的第三方 API 能直接替代原生远程连接。原生 Codex Remote 的账户配对不能直接替换成任意 Salcara Hub。[官方 Remote](https://learn.chatgpt.com/docs/remote-connections)

已开发独立 companion 并内置一键安装。测试未安装当前用户真实配置、未启用或连接真实桌面管道。只使用宿主实际授予的环境、真实执行元数据、审批和版本门控；不得扫描管道、获取其他进程凭据、伪造调用身份、绕过审批、修改 ASAR 或开启未经授权的调试端口。内部实现不是稳定的第三方契约。

活动实验在明确批准的 MCP 调用期间转发固定原生操作，loopback 随机端口及私有描述文件不上传 Hub。资源、权限或原生读取失败就禁用，不回退 CLI。仍需真实安装授权验证手机到原桌面的完整链路。

### 一键安装与只读 probe（connect 单独授权）

- 本地控制台接口受既有 loopback Host、cookie、同源检查保护；安装请求只接受 `confirmed:true`，不允许从 HTTP 指定配置目录、运行时或管道路径。无需模型 Key。
- 安装器仅复制资源包内 `package.json`、`LICENSE`、`src/*.mjs` 到 Bridge 私有数据目录。追加一张独立 MCP 表，保留所有旧配置字节和其他设置；同名非本安装器所有、已编辑的条目或安装途中配置改变均拒绝覆盖。
- 配置备份在写入内容前设置私有权限；Windows 使用仅当前用户与 SYSTEM 的 DACL。保存安装记录失败时，仅对仍等于刚写版本的配置回滚，后续用户编辑不覆盖。
- salcara_desktop_probe 默认检查环境信号，不读文件或连接 socket；salcara_desktop_connect 则需要单独明确授权。环境信号不是身份认证，还须固定资源哈希、真实上下文、用户批准、私有租约和会话白名单。
- probe 只读工具目录并返回 desktopControl:false / remoteSend:false；connect 才能调用固定 tools/call 白名单读取/续聊原会话。二者不扫描未知管道、不改 ASAR、不取消原任务、不输出私有凭据。
- 打包内置 Node 运行时及其完整许可证；配置使用绝对路径，不依赖尚未确认的 MCP 插件根变量插值。源码包与说明在 `remote/desktop-companion`。

## 验证说明

所有导航单测只构造 URL 或调用假回调，不调用 OpenCodex、真实深链接、桌面 UI 或当前用户会话。Claude 保护测试检查拒绝路径无文件及进程操作副作用。测试覆盖身份不一致、跨会话历史、未知执行器、桌面请求不降级 CLI 与保持 CLI 命令兼容。

源码需要重新构建电脑与手机，旧 exe / APK 不会自动获得本次功能。未部署公网或测试实体手机。
