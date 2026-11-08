# 远程技能增量与分页（2026-10-15）

手机首次读取完整技能目录；此后在相同配对、设备、Agent、账户、项目和会话作用域内携带已保存的目录版本，只传输新增、修改及删除项。目录没有变化时返回 `notModified`。原生刷新和网络传输是两个层次，下述协议不声称 Codex/Claude 已支持技能正文的增量扫描。

## 已核对的原生边界

- Codex app-server `skills/list` 的参数为 `cwds` 和 `forceReload`，返回 `data`，没有原生分页或 `sinceRevision`。本仓 `work/conversation-protocol-schema/v2/SkillsListParams.ts`、`SkillsListResponse.ts` 与 `internal/agents/codex_features.go` 是核对依据；原生 `skills/changed` 没有文件/项目身份。官方说明：<https://developers.openai.com/codex/app-server.md>。
- Claude SDK `reload_skills` 刷新原生定义，`SlashCommand` 返回名称、描述等元数据，没有源路径或正文摘要；既有核对记录见 `REMOTE-SKILLS-FRESHNESS-20261015.md`，SDK 0.3.300 来源：<https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.300.tgz>。Bridge 不扫描猜测的 Claude 路径，不伪造内容 hash。
- 30 秒缓存、Codex 变化通知及发送前原生重新校验仍生效。需要核对时原生接口可能整体 reload；技能正文单独变化时公开目录 revision 可以不变，发送仍使用重新校验后的原生技能。
- 目录 identity 是 `name + path`（Claude 没有 path）；revision 仅对规范排序后的展示元数据计算 SHA-256。原生枚举顺序变化不会制造目录更新。重名不同路径仍为不同技能，路径移动表现为新增和删除，不自动改绑用户选择。

## 协议

`features.list` 保留原身份/账户/来源授权字段，新增可选协商字段：

```json
{"syncVersion":1,"ifRevision":"<64位目录版本>","force":false,"pageSize":200}
```

`syncVersion: 1` 为必需的分页协商；省略时仍返回旧完整响应。`pageSize` 可省略，范围 1—200。`force` 强制原生刷新，但仍可依据版本返回差量或 `notModified`。已有源授权对整条命令签名，协商字段与分页游标同样受保护。

完整或差量页均返回：`syncVersion`、`kind: full|delta`、`revision`、`collaborationModes`、`skillsSupported`、`skills`、`removedSkills`、`totalSkills`、`totalChanges`、`offset`，未结束时还有 `nextCursor`。差量额外包含 `baseRevision`；其中 `skills` 是新增/修改项，`removedSkills` 仅包含待删除的 name/path。每页不超过 200 个操作和 256 KiB 编码预算，元数据及最终计数在各页一致。

后续页只携带 `syncVersion:1` 和 `cursor`（可带 pageSize），不能同时携带 ifRevision 或 force。游标为随机 transfer ID + offset，绑定完整授权作用域与不可变传输快照，2 分钟有效。重复同一游标可安全重试；其他账户/项目/设备作用域不能使用此游标。普通刷新产生新版本不改变正在传输的快照；原生变化通知、来源清除则撤销旧游标。

基线不在有界历史中时直接返回完整分页。手机收到不匹配的差量基线或游标过期时只自动重启一次完整同步，持续错误明确失败，不能把部分目录当作完成。

## 缓存、原子发布和限制

- Bridge 当前目录最多 64 个作用域、64 MiB；历史每作用域最多 3 代/10 分钟，所有作用域合计最多 64 MiB；分页快照最多 32 个/64 MiB。不同类别分别有界，淘汰会导致完整重取或明确游标过期。
- 手机最多 64 个作用域、32 MiB 已完成目录。`features.changed` 立即令缓存过期并使在途旧读取失效，但保留最近完整目录及其 revision 供差量基线；API/账户切换、配对或连接清除仍彻底丢弃对应缓存。
- 手机逐页验证版本、基线、偏移、计数、重复身份、重复游标、字段格式及大小。所有页收齐后才原子替换目录；失败保留上一个完整目录并抛出错误，下一次必须重新读取。晚到页不能覆盖新刷新，也不能恢复已清除的连接缓存。
- 移除了手机 `slice(0,1000)`。Codex/Claude 目录现在允许最多 10,000 项及 16 MiB；超过资源上限明确报错，绝不截取前若干项当作完整目录。展示描述、颜色和小图标仍受既有单项预算约束，不传技能正文。
- Hub 继续原样转发命令和结果，不展开/重排差量；features.list 仍为短期来源保护读取，不加入消息回执存储。

## 验证

- 新增 Bridge `feature_sync_test.go`：增删改只传变化项、1205 项跨页完整性及字节上限、游标重试/作用域隔离、分页中原生刷新仍保持原快照、全部删除和模式变化、历史过期完整回退、重复/超限拒绝、协商字段严格校验。
- 原生适配器：Codex 1205 项保留完整、超 10,000 明确失败；Claude 10,000、10,001 与超字节上限行为覆盖。
- 新增 Hub `feature_sync_test.go`：差量与后续页原样转发、游标/页大小/协商版本篡改不能复用来源签名。
- 新增手机 `remote-agent-feature-sync.test.ts`：原子合并、1205 项首次分页、全部删除/仅元数据变化、失配基线及过期游标完整重试、缺页/混合版本/重复项拒绝、连接撤销与新旧刷新交叉、10000 项新旧整批替换、旧协议兼容及超限显式失败。现有缓存和变化通知回归同步更新。

以上均为本地 mock/合成目录验证，没有真实模型调用、发布部署或修改另一对话的配置/控制台代码。验收日志由当前任务 `work/acceptance-20261015/skills-sync/` 保存。
