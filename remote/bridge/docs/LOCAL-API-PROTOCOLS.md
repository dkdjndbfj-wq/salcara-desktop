# 本地 API 服务

在 API 或官方账号卡片点击「加入 API 服务」即选择了该来源。首次加入自动建立本机服务；只有一个接入配置时，后续加入把新来源追加到这份配置。多个独立配置存在时需选择加入哪份配置，不会向全部密钥广播授权。未加入的来源不参与请求调度。

加入不会启动服务、启用已停用的配置、重置密钥或改动模型权限。原有主账号回退会保留；若使用固定成员策略，加入不会改掉固定成员。默认自动调度、模型授权、账号并发与故障处理继续由已有服务和网关负责。

管理页的「已加入 API」查看成员，「连接工具」提供本机工具入口与手动连接示例，「高级设置」管理独立密钥、权限、调度、网络与图片。独立客户端密钥只在创建或重置时返回一次；默认本机工具采用托管凭据，不需要手动复制密钥。

## 协议入口

默认根地址为 `http://127.0.0.1:<控制台端口>/codex-api`。已配置的专用 API 端口根地址为 `http://<连接主机>:<专用端口>`。专用端口不会开放本地控制台管理接口。

| 协议 | 相对根地址的路径 | 认证 |
| --- | --- | --- |
| OpenAI | `/v1/models`、`/v1/chat/completions`、`/v1/responses` 等已有路由 | Bearer 客户端密钥 |
| Anthropic | `/v1/messages`、`/v1/messages/count_tokens` | `x-api-key` 或 Bearer，二选一 |
| Gemini | `/v1beta/models`、`/v1beta/models/{model}:generateContent`、`/v1beta/models/{model}:streamGenerateContent` | `x-goog-api-key`、`?key=` 或 Bearer，三选一 |
| Ollama | `/api/tags`、`/api/chat`、`/api/generate` | Bearer 客户端密钥 |

OpenAI SDK 的 Base URL 包含 `/v1`；Anthropic SDK、Gemini SDK 的 `http_options.base_url`、Ollama Host 使用根地址。Gemini API 版本为 `v1beta`。模型命名空间和别名与 `/v1/models` 使用相同规则；Gemini URL 中的模型 ID 应整体 URL 编码。Ollama 客户端必须支持配置 Authorization 请求头。

Gemini 流式请求使用 `alt=sse` 返回 SSE，不提供该参数时返回逐步写入的 JSON 数组。Ollama 默认流式 NDJSON；`stream:false` 返回单个 JSON。响应中只保留实际报告的 token 用量，不补造总 token、模型文件大小、耗时或计费数据。中断和错误不会发出成功终态。

## 支持边界

Gemini 适配文本、系统提示、内嵌图片、函数声明/调用/结果、工具选择，以及温度、topP、输出 token 上限、seed、停止词和 JSON/schema 输出。单次只支持一个候选；安全设置、搜索/代码执行、文件 URI、思考签名等尚未支持的请求字段会被拒绝。同名并发工具结果必须携带可匹配 ID，避免错误关联。

Ollama 适配 chat/generate 的文本、图片、系统提示、工具历史、JSON/schema 输出，以及 temperature、top_p、num_predict、seed、stop。暂不提供模型下载、删除、运行列表、show/version、嵌入、原生 context token 数组、keep_alive、raw/template 或其它模型运行参数。不能用此服务代替完整 Ollama 模型管理服务器。

这是调用入口兼容；模型实际能力仍取决于所加入的 API。跨协议无法保留的参数会返回错误，不会静默丢弃后调用。未知原生字段、重复字段和歧义凭据会被拒绝。目录是当前接入配置允许的模型提示，不证明供应商已授予模型使用权限。

所有入口复用现有的运行授权、客户端停用/撤销、模型权限、明确成员池、并发准入、请求体限制和网关。Gemini 查询密钥不向上游转发，也不写入请求记录；日志路由使用模型占位符。浏览器 Origin/跨站来源仍被拒绝；托管工具专用凭据不能用来调用其它原生协议。
