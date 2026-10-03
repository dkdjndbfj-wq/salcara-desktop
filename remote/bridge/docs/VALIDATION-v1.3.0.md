# v1.3.0 本地验收记录

2026-09-30；Windows x64；Go 1.27.1；安装的 Codex CLI 0.145.0。
所有真实进程验证使用明确 fixture HOME/项目/配置，仅连接 loopback 合成上游。
没有使用真实模型 Key、收费推理、改写当前用户 Codex 或部署到线上。

后续桌面安全源码更新见 [DESKTOP-INTEGRATION.md](DESKTOP-INTEGRATION.md)：暂停不兼容 Claude Desktop 自动配置，明确区分 CLI/桌面，增加原 Codex 桌面会话导航请求。历史打包与下列原会话续聊记录不能证明完整桌面控制可用。

## 已通过

- config、launcher、toolcfg、gateway、hubclient、console 定向 Go 测试。
- Bridge go vet ./...；JS 语法检查。
- 手机 TypeScript；新旧 remote 3 个 Jest suite，19 个测试。
- 真实 Codex + Chat Completions：同一会话 2 轮、转换本地工具执行、完整工具结果回传，3 次合成请求，无 WebSocket 重试。
- 真实 Codex + Anthropic Messages：同上，保留非 OpenAI 模型 ID。
- WebSocket 固定账号快照、防切 Key 竞态；失败链失效、非法 previous ID、断开取消上游、流水线限流；新回归连续 20 次通过。
- 无模型 Key 的真实 Hub + Bridge 登记/SSE、QR PNG 实际解码、token-only 设备列表与 projects.list。
- QR 重放拒绝、手机 token 不能充当电脑身份、第二站点独立配对、跨站 token 拒绝、返回原站恢复身份、撤销绑定拒绝旧 token/QR。
- 浏览器桌面控制台：插件检测、已连接、二维码生成、4 个工具卡片；测试发现的连接状态按钮刷新已在源码修复。
- 插件登记/扫码并发、凭据哈希持久化、配置重载、撤销/轮换、隔离、IP 分桶；配套宿主 socket 来源覆盖定向测试。
- 真实 Codex 独立创建首轮后退出；Bridge 从同一 fixture HOME 读取原记录；手机模拟使用真实 Hub 配对令牌打开并向原 ID 续发，SSE 收到用户回显、最终回答及完成事件。仅一个会话，原历史/项目目录保留，3 次合成请求、0 付费请求、0 传输重试。复现脚本：tools/native-session-smoke.cjs。
- Codex 恢复前强制刷新原会话状态；读取失败/ID 不匹配/active 或 inProgress 拒绝续写，最近文件写入仍额外保护；新安全回归连续 20 轮通过，加固后再次通过上述真实原会话测试。

## 验证边界

- 手机网络/相机用 mock；实际 PNG 解码不等于实体 Android/iOS 相机验收。
- 本机独立 Hub 互通不是线上 Sub2API gRPC + TLS 反代/SSE 验收。
- 未测试用户真实 Grok/Claude 服务商质量、账单、模型参数和所有复杂工具。
- 未做完整 OpenAI WebSocket API、跨协议 remote compact、端到端加密。
- 手机选择 API Key/重启原工具未实现；Codex 桌面配置切换有 fixture 测试但非完整原生验收，Claude Desktop 旧写入方式后续发现不兼容，已暂停自动配置。
- 全量跨平台 Go 历史用例有 POSIX/macOS 路径及旧 agent diff 断言失败；不宣称 go test ./... 全通过。
- 原会话续写验证的是原 Codex 进程退出后的持久会话，不是抢占正在运行的原进程；手机打开的是会话记录，不是远程点击官方桌面窗口。
- 外部会话检测不是跨进程独占锁；另一个 app-server 的 idle/notLoaded 不能证明其他进程已经停止。用户仍需先结束原任务，不能在手机和电脑同时续写同一会话。
- 插件新增管理面板功能：本轮正在单独验收，最终结果需补充后才能视为通过。

当前脚本/测试源码为复现实验依据，生成的配置/凭据目录不应发布。
