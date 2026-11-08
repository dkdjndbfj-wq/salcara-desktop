# 桌面发布与自动更新

用户更新操作见 [使用教程](USER-GUIDE.zh.md#7-更新)。本页面向发布维护者。

## 信任身份

自 1.6.0 起，正式客户端固定使用 `dkdjndbfj-wq/salcara-desktop` 与 package.json 中的 Ed25519 公钥；1.6.4 保持不变。未配置公钥/通道的早期开发版需手动首次安装。

1.6.4 沿用同一身份，修复 Windows helper 交接、卸载后重装和远程会话分页。旧版更新器本身无法由尚未安装的新包修复；1.6.1 / 1.6.2 若内置更新失败，先手动运行新 setup。细节与测试边界见 [1.6.4 版本说明](RELEASE-1.6.4.md)。检查周期为 2 小时，提示和下载不停止任务，仅显式安装经过 idle/admission 检查。

发布私钥只在仓库外或 GitHub repository secret `SALCARA_DESKTOP_UPDATE_PRIVATE_KEY` 保存。不要运行 keygen 来替换已有身份，不要提交、打印或写入日志。更新签名不是 Windows Authenticode 证书或 macOS notarization。

## 发布步骤

1. 提高 `package.json` 和 lockfile 顶层版本，新增 `docs/RELEASE-<版本>.md`。版本必须为三个数字。
2. 手动运行 `Desktop source checks (manual)` 和 `Build Windows installer and signed updates`，使用同一精确源码提交。
3. Windows 流程执行桌面/companion/Go 测试、vet，构建完整 Electron 目录，编译 Inno Setup 安装器；在一次性用户中测试安装/重装/降级拒绝/卸载，再生成签名更新、便携 ZIP 和校验清单。
4. 两套检查均成功后运行 `Verify prior Windows build and stage release`，填入 Windows 构建 `run_id`、Linux 检查 `source_check_run_id` 与同一份 40 位 `source_sha`。下载产物前会核验两个 run 的仓库、workflow 路径、触发方式、成功终态和精确提交；不能拿另一提交的 Linux 结果代替。下载再锁定具体 artifact ID，复核所属 run / SHA / 名称 / 未过期 / 大小 / 摘要；流式下载限 2 GiB / 5 分钟，原 ZIP 的 SHA-256 和长度通过后才解压。不按可变化的产物名称重新选择，也不把“打印过摘要”当成实际校验。之后以被测源码的公钥和规则验证构建产物，创建同提交的 draft。任何查询失败或检查未完成都会拒绝暂存发布；失败的独占下载目录保留为证据，不自动覆盖或重试。
5. 产物验证通过后，流程通过创建 Git ref 原子预留精确源码的版本标签，再使用 `--verify-tag` 暂存 draft。任何已有标签（包括尚未建 Release 的标签）均拒绝复用，避免 `--target` 静默沿用错误提交。创建标签或暂存过程中出错会停止，不自动删除、强推或重试不确定结果；维护者先核实标签和 draft，再确定恢复方式。仓库仍应限制版本标签的强推/删除，不能阻止另一位有权限的维护者绕过流程改标签。
6. 检查源码、资产、签名、平台和测试记录后公开 draft。不得覆盖已有标签、使用 `--clobber` 或重新签发同版本资产。

一份 Windows x64 Release 必须有以下六个文件：

- `Salcara-Desktop-<版本>-win32-x64-setup.exe`：首次安装。
- `Salcara-Bridge-<版本>-win32-x64.zip`：便携下载。
- `Salcara-Bridge-<版本>-win32-x64.tar.gz`：应用更新载荷。
- `latest.json` 与 `latest.json.sig`：同一版本的更新清单与签名。
- `SHA256SUMS.txt`：包含其余五个文件的 SHA-256。

setup 和 ZIP 的哈希列在校验清单中；签名 manifest 指向 tar 更新载荷，不承诺 setup 已有 Windows 代码签名。GitHub 自动生成的 Source code 不是安装包。

## 普通更新与最低支持版本

签名 `latest.json` 可包含以下两个可选字段，仍使用 `schema: 1`：

| 字段 | 含义 |
| --- | --- |
| `minimumSupportedVersion` | 最低受支持的桌面版本，必须是三段数字，且不得高于本次 `version`。当前版本低于此值时必须更新；等于或高于此值时仍为普通更新。 |
| `updateReason` | 向用户说明兼容性变化的纯文字，最多 1000 字符。发布时填写此字段必须同时设置最低支持版本。 |

不设置这两个字段时，按普通更新发布。最低支持版本和原因与安装包信息一起签名，不能通过未签名的 Release 正文或任意站点响应触发全局强制更新。

例如发布 `1.8.0`，并停止支持低于 `1.7.0` 的桌面端：

```powershell
npm run release -- --notes ../../../docs/RELEASE-1.8.0.md --minimum-supported-version 1.7.0 --update-reason "远程协议已升级，请完成当前任务后更新桌面端。"
```

正式发布工作流 `Build Windows installer and signed updates` 提供对应的 `minimum_supported_version` 和 `update_reason` 可选输入。首次普通更新可以留空；维护者应在发布说明中同步记录最低支持版本、受影响功能和迁移办法。发布脚本读取上一版签名清单时，会保留其中的最低支持版本、原因和发布控制地址；工作流在全新输出目录构建时没有上一版清单，仍须显式填写要继续维持的兼容性要求。客户端读取最新 Release，不能在后续发布中无意丢失该限制。

合并同一版本的多平台 manifest 时，省略参数会保留已有策略；显式提供的值必须与已有值一致，包含原先未设置策略的情况，否则发布脚本直接拒绝。在添加其他平台时不能修改最低支持版本或原因。已有清单格式错误或属于其他仓库也会拒绝，避免静默丢失兼容性要求。公开后的版本和资产不得改写，策略变化应发布新版本。

支持此机制的桌面端确认必须更新后，会禁止开始新任务，保留已有任务、审批和停止的处理入口，并提示更新原因。更新窗口和首页共用小球样式，横向进度条显示下载比例、大小和速度。普通及强制更新均自动下载、校验和解压；任务或审批仍在进行时进入等待状态，每 10 秒检查一次，空闲后自动安装并重启，不再需要第二次点击确认。全程仍经过核心的空闲检查，不会自动结束运行中的 Agent。

关闭更新窗口只隐藏界面，下载和等待安装继续在后台运行；主界面常驻入口可以再次查看进度。「取消下载」会持久暂停这个版本的自动流程，再次点击更新可继续，强制更新限制仍然有效。下载连接最多尝试 4 次，单次无数据 30 秒超时，总时限 45 分钟，重试间隔 1、2、4 秒；支持强 ETag 的 Range/If-Range 续传，不支持续传的服务器会从头下载。签名、文件损坏和权限错误不循环重试。网络重试耗尽后保留可续传缓存并显示重试入口。任务繁忙时每 10 秒等待一次，安装身份等错误需用户重试。已验证的强制策略会缓存签名原文，重启时重新验签，离线或无效的更新响应不会解除限制。限制作用于 Salcara 管理的电脑和手机任务入口，不会禁用第三方应用自身的独立入口。

**历史版本迁移：** 1.6.4 及更早版本无法追溯显示新的强制更新界面。先保持旧协议可用，通过普通更新或安装包迁移到 1.6.5，再提升兼容性要求。配套 `salcara-hub` 的独立服务新增 `SALCARA_HUB_DESKTOP_MINIMUM_VERSION` 与 HTTPS 的 `SALCARA_HUB_DESKTOP_UPDATE_URL`，默认为不限制；设置后在命令派发前对旧版及版本不明的电脑返回 HTTP 426，保留配对、事件、已有回复、审批、停止及历史。不要在迁移前同时关闭旧协议和下载入口。

## 缓存、失败隔离和恢复

更新状态采用临时文件、fsync 与原子替换保存。压缩包缓存绑定签名版本和 SHA-256；重启后重新哈希并解压，不直接信任磁盘上此前解压的目录。下载前检查缓存盘和用户数据盘的空间，解压前按实际归档大小再检查，并预留 128 MB。

启动失败回滚后，失败版本持久隔离，定时检查和重新启动都不会自动重装；显式点击「手动重试此版本」才解除该版本隔离，新版本不受影响。健康启动才删除该次备份，失败诊断保留。Windows helper 在用户数据目录保留脚本和独立运行时，并登记以随机事务令牌命名的当前用户 RunOnce 项；正常完成或中止会清除该项。登录恢复发现应用目录缺失时先恢复精确的旧备份；发现已替换目录时检查健康回执。不会结束未确认健康但仍运行的新进程，其备份留待恢复。

## GitHub 分批发布与撤回

可选运行 `Publish signed desktop rollout control` 工作流，在独立的 `update-control` **预发布**中维护 `control.json` 与签名。它不会成为 GitHub 的 latest 应用版本。客户端只接受发布密钥签名、仓库匹配、未过期且修订号不倒退的控制文档；同一修订号不能换内容。

随后构建应用更新时勾选 `release_control`，或使用 `--control-url https://github.com/dkdjndbfj-wq/salcara-desktop/releases/download/update-control/control.json`，把地址放进签名清单。未启用时按普通 GitHub 更新工作；启用后无法取得有效控制就暂缓安装，已有任务继续。控制有效期为 7 天，需在到期前发布更高修订号。灰度按本地随机安装标识稳定分组，该标识不上传；强制兼容更新不受灰度比例延迟，暂停和撤回则对所有安装生效。安装前会再次拉取控制。

控制文件是允许独立更新的运维资产，应用版本标签和包仍不可改写。GitHub 更新两个控制资产期间若短暂出现签名不匹配，客户端会暂缓并重试，不接受未签名控制。撤回修复应发布更高应用版本。需备用下载源时，`release.cjs --mirror-base-url https://你的下载站/版本目录` 会把地址写进签名清单，所有来源仍校验相同的大小与哈希。

## 诊断与可选 Windows 签名

错误界面可复制更新诊断。记录只含更新时间、阶段、版本、错误类别、尝试次数和耗时，日志有大小上限，不含任务内容、路径、令牌或设备标识，也不自动上传。维护者可用 `node scripts/update-report.cjs diagnostics.json [...]` 汇总用户提供的成功、回滚和下载失败样本；这是样本统计，不是全体用户的成功率。

GitHub 分发默认不要求付费 Windows 证书，Ed25519 更新签名始终必需。有正式证书后可开启 `windows_signing`，配置 `SALCARA_WINDOWS_CERTIFICATE_BASE64` 与 `SALCARA_WINDOWS_CERTIFICATE_PASSWORD` secrets，工作流签署并核验桌面主程序、Go 核心和安装器，含受信任时间戳。启用却缺少证书时构建失败，不降级为无签名包。

## 安装布局与更新兼容

```text
%LOCALAPPDATA%\Programs\Salcara Desktop\
  unins000.exe / unins000.dat
  .salcara-installer
  app\
    Salcara Bridge.exe
    .salcara-install.json
    resources\
```

Inno Setup 仅为当前用户安装，payload 在独立 `app` 子目录。既有 updater 按 exe 所在目录整体替换，因此卸载器必须留在外层。配置和 Agent 记录沿用原 AppData/工具目录，安装与卸载不迁移或删除。

1.6.0 便携用户按原协议升级应用，不会自动得到 Windows 卸载条目；想改为安装版需手动运行 setup。安装器校验产品范围和版本，拒绝覆盖更高版本，不强行关闭任务。

## 自动更新保障和边界

- 固定仓库、标签、文件名；验证 Ed25519、版本、平台、实际下载大小与 SHA-256。
- 解压前全量检查 USTAR，拒绝越界、链接、设备、重复/冲突路径及异常大小。
- 本机自有核心空闲且审批已处理才能退出，与新任务共用 admission 锁；附着的非自有核心不被结束。
- helper 和安装许可准备好后才退出；新版本健康启动后随机令牌确认，失败回退或保留恢复备份，不强杀仍活着的未确认进程。
- 不扫描删除任意历史 sibling 目录；开发运行、不可写目录或无法验证的散装包转为手动下载。

Windows 安装器实测、合成目录替换和签名检查必须分别记录，不用 CI 打包成功冒充真实 Electron A/B 或手机链路验收。Mac/Linux 当前未提供正式安装包。最近的安装更新修复见 [1.6.3 维护记录](maintainers/RELEASE-1.6.3-REPAIR.md)，1.6.4 改动与验收见 [1.6.4 版本说明](RELEASE-1.6.4.md)。
