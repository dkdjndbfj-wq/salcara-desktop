# Codex API 服务配置接管 · 2026-10-15

本批独立实现 Cockpit 服务配置管理中此前缺少的一段：由 Codex API 服务明确拥有全局配置的备份、应用与恢复。未复制 Cockpit 非商业应用代码。它不等于任意外部启动的原生 Codex App 已取得实际 provider 来源证明。

## 已实现的行为

- 在“管理 API 服务”增加紧凑的“Codex 配置”入口。选择已有服务客户端和允许的默认模型，显式“备份并应用”；支持重新应用、刷新状态和恢复原配置。仅启动服务不会写入全局配置。
- 对 `auth.json` / `config.toml` 保存最初字节、原先是否存在、文件权限及 Windows DACL。使用服务专属 manifest、owner marker、跨进程文件锁和写入 journal，避免与 Workbench 的默认配置备份相互覆盖。
- 应用需要最新客户端 revision、配置 revision 和当前服务授权。文件提交并校验成功后才授予服务专属连接 Key；状态接口不返回该 Key。专属连接只接受 loopback 的 Models / Responses / compact 和 Responses WebSocket，不借用手动 Key 或 Workbench 授权。
- 每次重新应用有独立 generation；来源、客户端或服务授权变化使旧连接失效，重启后不会复活。原生文件被外部修改、owner 不符或备份不可信时拒绝覆盖，并显示明确错误。
- 停止服务先撤销执行授权，再恢复服务拥有的配置；正常退出也尝试恢复。配置保存失败不能恢复运行权限。崩溃或强制结束留下可恢复 journal，下一次通过明确恢复操作处理，不静默改写用户文件。
- 正常退出先设置不可逆的请求关闭标记。已进入处理、但尚在读取请求体、排队、初始化或刷新凭据的请求会再次检查；迟到的设置写入即使重新发布旧运行状态，也不能让已关闭的服务实例重新接单。已转发的请求继续排空。
- API 启停、网络设置、图片模式与配置事务共用生命周期锁。Workbench 的 Codex 写入和恢复共用 profile 文件锁，任一方拥有配置时另一方拒绝叠加备份。
- UI 对关闭后迟到响应、重复点击、未知写入回执和重新打开面板做了隔离。外改冲突时禁用应用与恢复，不提供强制覆盖入口。

## 接线与验证

主要实现为 `internal/toolcfg/codex_service_profile*`、`internal/config/service_profile.go`、`internal/apiservice/managed_launch.go`、`internal/console/codex_api_profile.go` 及 `web/codex-api-profile.*`。

接口：

- `GET /api/local/codex-api/profile`
- `POST /api/local/codex-api/profile/apply`
- `POST /api/local/codex-api/profile/restore`

所有配置测试使用临时目录和合成凭据；未读取或改写用户真实 Codex 配置，未进行真实 OAuth、额度消费或付费推理。

| 检查 | 结果 |
| --- | --- |
| 最后关闭修复后的全包普通测试 | console 95.242s、apiservice 3.079s，通过；其余受影响包未再修改，保留已通过的全包结果 |
| 文件事务最终普通 / race | toolcfg 33.143s / 43.501s，通过；包括 Windows ACL、实际文件占用、各写点中断、外改和旧恢复回执 |
| 服务配置和网络生命周期最终 race | console 30.799s、launcher 4.261s，通过；包括启停、重新应用、网络设置、图片模式、退出恢复及旧配置回滚 |
| 不可逆关闭准入最终普通 / race | apiservice 定向 0.368s / 1.613s，通过；包括相同运行状态迟到重开和 managed launch |
| 启动器全包 | 23.771s，通过；Windows 快照只对持有 byte-range lock 的空锁文件记录其元数据，其余配置仍按原字节校验 |
| 五个受影响包静态检查 | `go vet ./internal/console ./internal/launcher ./internal/toolcfg ./internal/config ./internal/apiservice` 通过 |
| Linux amd64 | 五个受影响包交叉编译通过 |
| Windows amd64 | Bridge 整树编译通过，未执行该二进制；本批 32 个源文件验证前后 SHA 相同 |
| UI 专项与启动兼容 | 合计 63 项通过，其中新增配置面板 5 项；没有因 UI 操作失败自动重复提交写请求 |
| 浏览器实际 UI | 1280×720 应用、320×740 恢复/深色外改冲突通过；无横向溢出，控制台 0 error / 0 warning |

UI 验证使用真实新增 JS、共享样式、面板和选择器配合合成后端，未连接真实账号。截图与日志位于本次工作目录的 `output/playwright/service-profile-20261015/`。本批 32 个源文件哈希记录在 `output/service-profile-source-20261015.json`，不代替另一会话正在进行的整树冻结验收。

独立编译证据 `output/salcara-bridge-service-profile-20261015.exe` 为 75,729,408 字节，SHA-256 为 `c5860592ac37a8d6de44dcf9727cb844af355d014eb776a7c4c4588a721e5a54`。协作会话负责新的整树三端验收包，此文件不作为已完成安装/签名验证的发行包。

## 明确保留的边界

- 两个文件分别原子替换。遵守共享锁的 Salcara 写入方互斥；不承诺不遵守锁的外部进程在每个瞬间都读到一整组配置，也不承诺断电或硬件故障下的持久性。
- 外部修改冲突或退出时配置事务未及时结束会保留备份/恢复记录并报错，不强行覆盖。配置恢复只处理本服务明确拥有的文件。
- 本功能不证明任意原生进程的实际 API 来源，也不开放此前被来源保护拒绝的原生远程写控制。
- 真实 OAuth、站点 issuer、额度/套餐/重置消费及实体手机仍需目标环境验收。此批不部署、不签名发行；旧阶段安装包不包含本次新增代码。
- 侧问持久化、技能分页与逐行批注由协作会话整合，不能用本批局部测试替代这些新功能的验收。SIWC 及跨软件共享 refresh token 的仲裁继续按用户要求暂缓。
