# 原生 Codex 桌面模型切换核查（2026-10-12）

本次只补原生模型链路，不发布、不部署，不改变手机或桌面视觉样式，不使用真实账号、API Key 或真实宿主授权。全部测试是本地合成 HTTP/命名管道替身。

## 修复范围

- Companion 的 `tools/list` 仍默认仅返回安全工具名；原生连接额外读取确切 `codex_app.send_message_to_thread` 参数 schema。仅明确、简单的 `threadId`、`prompt`、可选 `model` 字符串合同启用模型覆写。重复工具、复杂组合、引用、额外必填参数或受限模型 enum 均保持关闭。
- `thinking` 必须单独声明有限英文 enum；支持 `model` 不等于支持所有推理强度。对外只暴露有界能力投影，不传播宿主 schema、描述或内部路径。
- 新能力合同为 `modelOverrideTransport: codex-tool-schema-v1` 与 `modelOptions: { model: true, thinking: [...] }`。旧宿主、旧适配器继续使用原有纯文本发送签名；不可丢弃指定选项后降级发送。
- 新的可选 `NativeSendWithOptions` 路径把 `model` 原样传给宿主 `model`、`effort` 原样传给 `thinking`。宿主 schema 在新派发前再次读取，缺失或失败明确返回未派发；已完成操作只重放回执。
- 操作摘要包含模型及强度。相同 operation ID 改模型、改强度、改正文会冲突，不新发第二个任务。
- 原生目录独立处理 `models.list + controlSurface=desktop + sessionKey`，必须有当前授权会话和宿主模型能力；从桌面实际 `AuthorizedAppliedToolAccount(codex-desktop)` 读取，不能使用手机后台 CLI 的 `RemoteAPI`。普通 API 目录仅是上游候选提示；官方账号保留原生/已保存 hint 的来源说明，均不当作可调用或套餐权限证明。
- API 来源、目录策略、launch lease、会话授权在等待前后复核。冻结来源回调在 Go 原生服务等待 status 后、发送 gateway RPC 前再次执行，不被服务重新采集的新来源替代。
- 手机 status 中普通 `api` 仍表达后台 API；`desktopLive.accountId/apiName/apiModel` 只表达桌面已应用 API。旧 `codex-desktop` 卡的 `api.accountId` 同样使用已应用账号的 opaque 句柄，不泄露 vault ID、端点或密钥。
- 生产 `NativeSourceIdentity` 改为只依赖已应用桌面来源和 launch lease 的 SHA-256，不再优先使用 CLI 的 RemoteAPI。CLI 切换不会误换原生身份；实际已应用源变化必须重新授权。
- 已配对手机的原生 send/approval 账号预检查同样使用已应用 API。原有受限 Hub 来源验证仍保留，模型能力不能充当原生实际 provider 的证明。

## 涉及文件

Companion：`src/native-catalog.mjs`、`src/gateway.mjs` 及相应 catalog/gateway/native-control 测试。

Bridge：`internal/desktopcompanion/active.go`，新增 `native_models_test.go`；`internal/hubclient/native_models.go` 与测试，`dispatch.go` 原生路径和顶层账号验证小块、`aggregation.go` 原生能力分支、`agent_status.go` 安全投影；`main.go` 来源回调，新增 `internal/config/native_source.go` 及测试。旧能力拒绝测试调整为检查实际拒绝且未降级，未删除安全断言。

## 本次验证

- Companion 全量 `npm test`：101/101 通过；进一步严格 schema 根字段后重跑受影响 catalog/control/gateway：37/37 通过。
- Go focused：desktopcompanion、hubclient、config 三包通过，覆盖旧宿主、非法模型/强度、相同操作选项冲突、Applied A/Remote B 分离、来源变更、宿主失败不得吞掉、原生权限覆写仍拒绝。
- Go `-race`：上述三包的 native/model/scope/source focused 测试全部通过。
- Go `vet`：desktopcompanion、hubclient、config 三包通过。
- 针对本次文件的 `git diff --check` 通过（Windows CRLF 按 cr-at-eol 处理）。
- 较早三包全量曾发现旧 `models.list` 错误文案断言及并行权限改动的默认 options 路径断言；前者已按新原生 scope 拒绝更新且 focused/race 通过，后者由主任务修复并统一做最终全量，不能把这里的 focused 结果当成全部 Bridge 验收。

## 明确边界

1. 未使用真实 Codex Desktop 做授权、重启和模型实发验证。安装的宿主必须在运行时提供兼容 schema；否则保持关闭，不能承诺所有版本均可切换。
2. 目录不是模型权限。宿主不接受某模型时保留失败/不确定回执语义，不自动换模型、不改 CLI 重试。推理强度的模型级支持未知时，手机应保守隐藏，不凭宿主通用 enum 猜测。
3. 原生权限模式、附件、停止能力没有随模型支持一起虚假开启；受限 Hub 原生来源证明缺口仍按既有策略拒绝。
4. Companion 安装器禁止同版本不同 payload 覆盖。当前未发布工作版本 0.4.2，Git HEAD 为 0.4.1；如果测试者已安装过另一份 0.4.2，正式打包前需统一递增 Companion 版本，不能强覆盖旧目录。

参考：OpenAI 官方 app-server 的 `model/list` 是模型目录合同，不代表原生桌面 MCP 调用合同，也不证明账号可用性。实际原生能力只认本机运行时 `tools/list`。

<https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server>
