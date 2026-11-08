# 官方账号美元估值补齐（2026-10-13）

## 产品口径

- 金额是 **API 等值估算**，不是订阅价格、官方余额或可提现/可消费金额。
- `usedUSD` 保持旧协议的“本机留存已定价小计”，`localUsedUSD` 是同义明确字段。
- `windowUsedUSD / totalUSD / remainingUSD` 严格来自同一原生额度窗口、同一种估算；不能用留存小计加剩余推导总量。
- `windows.primary / windows.secondary` 分别对应上游窗口，独立标定、保存和失效。不得相加（两个窗口计量同一批请求）。顶层仍取 secondary，缺失时取 primary，供服务池去重汇总。
- 每个窗口都有 `freshUntil / windowResetAt / windowSeconds / priceBasis / reason`；未知金额是 `null`，明确零才能显示零。

## 自动价表

`internal/usageledger/reference_pricing.go` 内置带日期和版本的只读 OpenAI API 参考价表。新请求无需用户先填写费率；精确模型和计费层级命中才估算，不裁剪模型后缀或把未知模型映射成类似型号。

- 官方来源：<https://developers.openai.com/api/docs/pricing>（含实际读取的 `.md` 表格，2026-10-13）；补充 <https://developers.openai.com/api/docs/models/gpt-6.1-sol>、<https://developers.openai.com/api/docs/models/gpt-5.2-codex>、<https://developers.openai.com/api/docs/models/gpt-5.3-codex>。
- 缓存写入计数及互斥计费：<https://developers.openai.com/api/docs/guides/prompt-caching>。Collector 读取 `usage.input_tokens_details.cache_write_tokens`，新版模型缺失计数时不假设免费。
- 区分 `official_reference` 与 `user_pricebook`，保留版本。用户对应模型/层级的覆盖项优先；覆盖项空白保持未知，明确 0 为零价；不回退覆盖其意图。
- `GET /api/local/usage-ledger/pricing` 的 `entries` 仍是用户覆盖，新增 `reference` 含来源、核对日期和参考条目；保存不偷偷重算历史。既有“重算历史估值”仍需明确操作。
- 未包含服务商加价、地区上浮、工具调用费等；不能作为真实扣费凭据。公开纯函数 `EstimateReferenceCost` 为后续请求时计费适配提供参考金额，不能把服务商当前倍率倒算到历史。
- 参考表随软件版本更新，不会在每次请求时联网抓网页。

## 剩余和总量的估算

1. **初步窗口推算**（`observed_window_api_equivalent`，`partial`）：至少一条本窗已完成、完整定价请求且官方已用比例大于 0，按已记录窗口成本 / 已用比例推算总等值。外部客户端使用或缺失记录可能使该值偏低，不能声称账户余额。
2. **双快照学习**（`sampled_api_equivalent_not_balance`）：同身份/套餐/窗口至少两次官方快照，区间至少 3 条完整定价请求和 1 个百分点消耗，用增量成本 / 增量百分比校准。价表、记录覆盖、身份、窗口、来源集合变化使旧标定失效。活动请求和额度读取交错时暂不计算。

Cockpit 参考来源为其公开 `src-tauri/src/modules/codex_local_access_routing_pricing.rs`（main，2026-10-13 只读）和 1.3.59 Release 对“窗口成本 / 使用比例”推算的说明；独立实现，未复制其 CC-BY-NC-SA 源码。没有宣称与 Cockpit 全部算法等价。

## 持久化、隔离与汇总

- 学习标定存入既有私有 SQLite 元数据，仅匿名身份键、来源集合哈希、价格版本和数值，不含账号名称、Key、token 或请求正文。
- 正常退出后可恢复同窗口标定；异常退出可能丢失已消耗额度但未记录的在途请求，因此只保留普通请求记录，清除不可靠标定。
- 清空、保留范围变化、重算或价表修改使标定失效；迟到旧身份/旧价格写入不能复活标定。
- 标定写队列有界，不在账号卡请求内等待磁盘，不阻塞推理；写失败不会伪造持久成功。
- 服务池先按 credentialRef + identityEpoch 去重。同账号多客户端/多本地句柄只计算一次。
- 服务池金额描述为“各账号窗口合计”，不伪装成同一个周期。任何成员窗口估值未知时，总数维持 `null`，同时返回已估算数量；未知不得按 0 加总。

## 验证

- 2026-10-13 首轮全量：`go test ./internal/usageledger ./internal/console` 通过（13.482s / 54.205s）。
- 定向覆盖：冷启动价格、层级未知、缓存写入、长上下文、用户空白与零覆盖、持久化来源、正常重启、非正常重启、旧身份、清空、价格变更、同窗三数、初步估算、未知池成员、独立双窗口。
- 加入双窗口前 race 关键集通过（usageledger 8.468s / console 3.849s）；最终双窗口、标定恢复和池汇总窄 race 连跑 3 次通过（console 2.200s），双窗口定向测试通过（1.761s）。`go vet ./internal/usageledger ./internal/console` 通过。
- 最后一次全包复跑：usageledger 通过（30.402s）；console 达到 120s 整包超时，当时 `TestManagedSwitchPublishesOnlySavedUnchangedSource/save` 仅运行约 1s、堆栈在 Windows 文件原子替换，未出现本轮估算断言失败。此结果不能记作最终全包通过；根任务需合并后续全量结果。
- 后续根任务合并复验：console / relay / usageledger 整包分别 89.771s / 2.241s / 19.237s 通过；刷新／清理修复后 console 再次整包 76.790s 通过（600s 总预算）。前述 120s 超时保留作为历史记录，不再是最终验收状态。卡片尺寸、分组倍率与网络失败排除的完整记录见 [ACCOUNT-CARDS-BILLING-CLOSURE-20261013.md](ACCOUNT-CARDS-BILLING-CLOSURE-20261013.md)。
- 所有验证均使用合成请求和临时私有文件；未调用真实账号或付费模型，未发布。

## 仍须正确表达的边界

上游若没有提供 token 分类、可信额度百分比/窗口/时间，或者模型不在明确参考表中，仍显示未知；这是信息不足，不允许编造。新导入、未产生任何可观测请求的账号没有可学习美元预算。手动修改费率不会将新费率自动覆盖旧估值。账本只记录经过 Salcara 的调用，不能还原其他软件所有历史。
