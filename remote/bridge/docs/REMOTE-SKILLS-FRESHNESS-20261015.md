# 远程技能目录刷新与来源撤销收尾（2026-10-15）

后续增量/分页及缓存语义更新见 [REMOTE-SKILLS-SYNC-20261015.md](REMOTE-SKILLS-SYNC-20261015.md)：技能变化现在使手机旧目录过期并保留差量基线，完整收齐前不清空可见目录；API/配对来源切换仍彻底清除缓存。

本轮范围是 Codex 技能变化通知、手机目录与菜单的旧结果隔离，以及 Claude Code 的 API 接入。SIWC 仍暂缓。没有部署、运行真实授权流程或调用付费模型。

## 原生协议与边界

- Codex `skills/list(forceReload: true)` 从原生目录重新读取；`skills/changed` 是空通知，没有 cwd、任务或文件身份。依据：<https://developers.openai.com/codex/app-server.md>。
- Claude Agent SDK 0.3.300 的 `reloadSkills` / `reload_skills` 从磁盘刷新原生技能。返回的 `SlashCommand` 只有名称、描述、参数提示及可选 aliases/builtin，没有路径或正文摘要。依据：<https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.300.tgz>。
- Bridge 与手机缓存的是展示目录。公开 `revision` 是目录元数据摘要；正文单独改变时它可以保持不变。缓存清除与发送前原生 reload 是不同步骤，不把 revision 冒充正文 hash，也不读取客户端提供的任意路径。

## 刷新链路

1. Codex 通知处理绑定具体 RPC 连接，只接受当前 worker 或有效启动中的 worker。读循环仅发布失效信号，绝不在回调内等待 `skills/list` 响应。
2. Manager 接受具备 CLI Features 能力的已注册 adapter；输出纯 `features.changed` 控制事件。禁止携带会话，清除正文、附件、审批等无关字段。
3. Hubclient 在上传资格检查之前清除对应 adapter 的全部目录缓存，含其它 cwd/session 下的条目。在途旧读取不能覆盖新缓存；其它 adapter 不受影响。未上传的重复控制事件可合并，不改动已在上传的批次。
4. Hub 从有界账户事件环提取当前设备每个已注册 tool 最新控制信号，通过 `/app/events` 的可选 `featureChanges` 带外字段返回。正常会话 events、游标和 gap 语义不变；不存在会话及 reset 页也保留信号。
5. 手机在普通页和 gap/reset 分支前消费控制快照；按连接/配对、设备、tool 和 Hub seq 去重，清除缓存并更新当前目录代次。旧请求、旧菜单回调及渲染尚未跟上的点击均需重新检查代次。输入草稿和所选技能身份保留，不把同名技能自动改绑到新路径。

实际分页示例（严格四字段，无 sessionKey）：

```json
{"featureChanges":[{"type":"features.changed","tool":"codex","deviceId":"pc","seq":1001}]}
```

原始 Bridge/SSE 控制事件仍具有 `sessionKey: ""` 与 `ts`。手机分页解析允许省略 sessionKey，显式 null 或非空值仍拒绝；SSE 要求显式空值。

长轮询仅由本次等待期间的新信号提前唤醒，旧快照不会产生空转。Hub 已有持久序号预留机制，同数据目录重启后序号继续增长；无 DataDir 的临时实例丢失配对，重新配对会更换手机作用域。

事件环淘汰、断线或尚未创建会话时不保证即时送达。保留 30 秒目录核对、入口/前台刷新和发送前原生重新读取。

## Claude API

图片读取提前到技能 reload 之前，缩短原生 reload 成功至 user frame 的间隔。每次技能发送仍重新 reload，并保留发送前取消、worker 及权限检查。builtin 和所有同名重复项不再被当作可精确选择的技能；失败或已删除技能不能沿用上次目录发送。

## 来源撤销复核

额外 mock 复现发现既有 ABA 边界：桌面配置在待发请求 reload 期间 A→B 时，旧 worker 请求被网关拒绝；A→B→A 原样恢复时，旧派生凭据却重新匹配。手机 API 切换与发送的共享锁不能覆盖桌面直接 Store 更新。

远程 worker 派生凭据现包含对应目标的 Store 授权代次。A→B→A、撤销后恢复及策略恢复都产生新凭据，旧 worker 不会恢复有效。授权状态比较先使用 Store 旧代次，避免回调篡改或每次无关保存引发循环轮换；旧快照与其它路由保持隔离。

原 native reload 竞态 mock 在修复后，单向切换与 A→B→A 均为 HTTP 401、上游调用 0。仍观察到一个 native user frame，实际阻断点是网关；不宣称消息在进入 worker 前就被拒绝。新 worker 使用已提交的新凭据可正常访问 mock 上游。

## 已完成的验证

- Bridge 技能相关普通测试通过；通知、缓存、相邻启动/关闭及聚合事件的 race 测试通过（agents 25.345s，hubclient 2.240s）。
- Claude 技能及相邻发送/取消/权限回归与 race 通过，检查了正文变更、同名移动、删除、刷新拒绝、内置冲突、附件先读和最终取消。
- Hub 全套普通测试及全套 race 通过（最终 race 31.804s），全仓 `go vet ./...` 通过。新增测试覆盖设备/tool 隔离、分页/reset、长轮询、批次重试及同配对重启。
- config/gateway 全套普通与 race 通过（race 3.356s / 9.657s）。Store 代次私有、不落盘、clone 隔离、保存失败回滚、无关更新不轮换等既有约束仍通过。
- 手机最终控制/store/cache/历史/原生隔离 5 套 112 项通过；ThreadView 技能筛选与通知相关 49 项通过，最后清理按钮意图保护另有 8 项及跨来源 1 项回归通过（有重叠，不累加）。TypeScript 检查通过。没有运行完整 ThreadView 套件。
- 多轮只读复核修复了手机缓存命中后的微任务竞态，以及 HTTP 四字段快照与 SSE 格式误用。最终 Bridge 交叉复核未发现确定的新缺陷。
- Bridge 全仓 `go test ./... -count=1 -timeout=300s` 与 `go vet ./...` 通过（含 agents 236.722s、console 89.528s、hubclient 60.349s）。

手机详细证据：`C:/Users/24230/Documents/Codex/2026-10-14/codex-threads-01a0ae56-e9f6-7933-9ad4/outputs/MOBILE-SKILLS-CHANGE-20261015.md`。该会话的 `outputs/skills-change-20261015/manifest.json` SHA256：`3d04ea9432797cf40e6d6770104ffdb7755aa3aaf8391b82d20174e7c01ad4d2`。

三端本轮 25 个生产/测试文件的验证快照 SHA256 清单：`C:/Users/24230/Documents/Codex/2026-10-10/codex-threads-01a0ae56-e9f6-7933-9ad4-2/work/skill-refresh-final-manifest.json`。手机源码在交接时已重新核对，与协作会话冻结清单一致。后续协作会话新接到的技能名称视觉及消息显示任务使用独立基线，不计入本轮技能刷新验证。

测试使用 mock worker/upstream；这些结果证明协议顺序、缓存和授权围栏，不代表真实模型正文执行或真机链路已经验收。
