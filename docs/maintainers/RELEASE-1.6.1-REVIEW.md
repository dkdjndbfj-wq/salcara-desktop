# Salcara Desktop 1.6.1 改动与验收记录

## 范围

基线为正式 `main` 的 `8300d958e8e7964348adf94a21571b18179fd0ae`。仅整理公开文档、增加 Windows 安装器与安装回归、同步版本和发布资产验证。手机、Hub、服务器不在本轮范围。

正式产品继续使用 `dkdjndbfj-wq/salcara-desktop` 的 `main`。不发布第二个桌面仓库或另一套补丁项目；既有本地未提交工作保留，不混入发布。旧开发来源说明从用户首页移除，历史验证整理到维护目录。

以下保持不变：所有桌面 HTML/CSS/web JS、图标、UI 文案、动效；Electron main/preload/updater 与 Go Agent 内核；本机真实 API/配对/Agent 配置；更新 repo 与 Ed25519 公钥。应用内部名称仍为 Salcara Bridge，避免变更数据目录及既有配置。

## 文件与行为

- `README.md`：产品介绍、直接下载、快速入门、远程配套、更新/卸载、能力和许可。
- `docs/USER-GUIDE.zh.md`：API/Agent/模型加载、Claude 第三方模式、会话恢复、Hub 配对、更新、卸载与排错。
- `docs/DEVELOPMENT.md`、`AUTO-UPDATE.md`：开发和发布步骤，与用户教程分开。
- `docs/RELEASE-1.6.1.md`：用户发行说明；旧过程交接文档整合为 `maintainers/RELEASE-1.6.0-VERIFICATION.md`，保留真实历史证据。
- 旧实验指南及 companion README 增加历史提示，不删除兼容实现或许可证。
- `installer/windows.iss`、`root.marker`：Inno Setup 当前用户安装，固定专属路径；完整 payload 放 `app`，外层保留卸载器，开始菜单和可选桌面快捷方式。
- 重装先将已验证旧应用重命名到专属 `.salcara-setup-recovery`，新文件成功安装后才清理；安装失败尝试恢复，无法安全恢复则保留备份；已有恢复目录时拒绝覆盖。安装/卸载拒绝被占用或含重解析点的 payload，不强杀进程；不清扫用户父目录和 `app.old-*` 恢复备份。
- `scripts/installer.cjs`：校验生产版本、完整产品标记、必须文件、无链接/特殊节点后调用 ISCC，不启动 App、不读签名私钥。
- `scripts/package.cjs`：随包附带产品首页及根 `docs` 的新用户教程，代替旧 Bridge 过程文档；发布验收要求教程真实存在。
- `scripts/test-installer.ps1`：仅 GitHub Windows runner，一次性用户/profile 的真实安装器回归，不启动 Agent/App、不触碰开发者配置。
- `test/installer.test.cjs`：合成载荷/调用边界测试；`update-hardening.test.cjs` 增加安装版 parent/app 布局，确认更新/回滚不删除外层卸载器。
- `package.json` 与 lockfile：1.6.1，加 `installer` 命令，依赖和签名公钥不变。
- `desktop-release.yml`：安装器构建及独立用户 smoke，六文件统一校验，签名 secret 只导入临时签名步骤。
- `verify-release-artifact.cjs`：资产清单从五个增至六个，setup SHA256 与 Windows PE 验证；tar 签名协议不变。
- `desktop-publish-verified.yml`：使用确切被测源码自身的 verifier，不再从当前 main 替换；草稿显示 Salcara Desktop 产品名。

## 已验证（本机）

- Windows desktop Node：42 项，41 pass，0 fail，1 POSIX e2e skip；含完整合成 Windows swap/rollback，外层卸载器保留。
- Windows companion：93 pass，0 fail/skip。
- Windows Bridge：与既有发布相同的 `-short` + 五项平台断言排除测试通过，`go vet ./...` 通过；没有修改 Go 运行源码。
- 本机未安装 ISCC，真实 setup 编译/安装器执行由 Windows CI 验证，未在本机安装覆盖正式 App。
- CodeRabbit 审查技能：CLI 缺失后尝试官方安装命令，返回 `Unsupported operating system: mingw64_nt-10.0-26200`。没有启动审查、没有结果，不以手工审查替代或声称其通过；需要受支持的 Linux/macOS 环境安装、登录后才能补做。

## 发布门槛与边界

发布前须完成真实 Inno 编译及隔离用户安装/重装/降级拒绝/锁定与链接拒绝/卸载/配置和恢复数据保留测试，Linux source-check、六个资产的 SHA256、Ed25519 更新签名和完整 ZIP 检查，并核对 UI/内核 diff 和敏感文件清单。

不能把安装器 smoke、合成进程或 CI build 当真实 Codex/Claude/手机全链路或真实 Electron A/B 更新验收。setup 没有 Windows Authenticode 签名；此版本不声称消除了全部隐藏问题。

## GitHub 首轮结果

- 提交 `f9f5acd27c6f2f261267c72ffab4a73f5c2a8c57`。
- Linux source-check `37198632957` 成功：desktop、companion、Go short 单元、vet、race。
- Windows build `37198628697`：全部源码测试/vet、完整 Electron 构建、真实 Inno Setup 编译成功；隔离用户 smoke 失败，worker 未生成结果。流程拒绝签发上传或正式发布，未把“编译成功”写为“安装验收成功”。
- 后续修复首先补隔离 worker 的诊断及初始化失败返回，不绕过账户隔离或直接在真实用户环境安装。

第二轮 `37199287625` / 提交 `40b9d487ebd364ae24188a0478526b2bca0f6a80`：完整构建、Inno 编译再次成功，诊断正确返回隔离 worker 的 `GetFullPath` 参数为空/非法路径异常；尚未通过实际安装验收。Linux source-check `37199291611` 成功。补创建已验证一次性 profile 的 AppData 目录后再核对 known folders，不跳过隔离检查。

Git HTTPS 上传中一次连接重置导致调度到旧提交，Windows run `37199152820` 已取消，不计作新补丁验收；源码通过官方 Git database API 保持精确 SHA 和非强制 fast-forward 后，才调度上述第二轮。重复旧源码 Linux run 不替代新源码验收。

后续 CI 与正式资产结果在实际完成后追加。

第三轮 `37201878907` / 提交 `41822ad5a3d835f22012dd2de7c33360e7ba050a`：编译成功，隔离 worker 的 known folders 与新用户 profile 不一致，安装仍未开始、未签发或发布。Linux `37201882251` 成功。测试启动修复为先调用 Windows `CreateProfile` 创建一次性 profile，再通过 PowerShell 7.4+ `Start-Process -Environment` 在进程启动前传入该用户的环境；保持 SID、注册 profile、known folders 的一致性校验。该参数默认不会自动为不同凭据切换继承的用户环境，参考 [Microsoft Start-Process 文档](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.management/start-process?view=powershell-7.5) 与 [CreateProfile](https://learn.microsoft.com/en-us/windows/win32/api/userenv/nf-userenv-createprofile)。

第四轮 `37202372796` / 提交 `61fa07e37c1861932d2a4db130b21fff54c8aba0`：独立调用 `CreateProfile` 被 runner 拒绝，未运行安装器；Linux `37202376364` 成功。去掉该额外调用，使用已经可用的 `LoadUserProfile` 建立 profile，启动前指定随机且原先不存在的账户目录，worker 仍必须与系统 SID 注册和 known folders 完全一致。增加 `-ProfileOnly` 前置快速验收，目录初始化失败时不再先重做完整压缩；正式安装器回归仍是独立必须步骤，没有取消。

## 最终构建与验收

- 完整应用及安装器源码：`5e267fcb5adb0fee162ed96b98a9ac58764c178a`。
- Linux source-check `37202829860` 成功，包含全部 Linux desktop/companion 单元、Go short、vet、race。
- Windows build `37202827043` 全流程成功：Windows 单元/vet、独立用户环境、完整应用与 Inno 编译、真实安装器回归、签名 tar、便携 ZIP、六文件哈希和产品/版本/USTAR/PE/教程完整性验证。
- 实际安装器在一次性普通 Windows 用户内执行，没有启动实际 App/Agent。安装文件 SHA256、HKCU 卸载版本、开始菜单和桌面快捷方式目标一致；同版本重装成功。其他安装路径、锁定文件、较新 payload、junction、已有安装恢复目录均拒绝覆盖。卸载删除新增 payload 文件及自己的卸载条目，保留三种配置目录、父目录额外文件、更新/安装恢复备份；仅删除精确本安装的自启动值，第二次卸载保留其他安装的自启动值。
- 合成 Windows updater 成功交换/启动确认与失败回滚均通过，外层卸载器保留。**Inno 中途文件复制失败恢复机制未做真实故障注入**；不把这项实现写成已经实测通过。
- 签名更新 tar：199914876 字节，SHA256 `b743f0d232187f99d563a9f216bd52524dcd97e7e5abfb4f337449b6d1ed3043`。
- setup SHA256：`da8191014c643398ccca038c67255dd14d18dc7a39996552a7aec0994f25c666`。
- CI artifact ID `11303576980`，547106564 字节，digest `sha256:df5adefffe964f540671d2baa432bbd0d016852f41e64f7653a213b802d0e071`。
- 与基线相比，所有 UI/动效文件、Go 运行源码、Electron main/preload/updater、品牌资源 diff 为空。提交清单没有真实配置、私钥、环境文件、缓存或服务器部署数据。

额外公开前验收 [37203244709](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/37203244709) 成功：下载上述确切成功 run 的 artifact，并用上述被测源码再次完整验证后创建草稿。维护记录后续提交仅改文档，不替换上述应用构建身份。

## 正式发布与公开下载验收（2026-10-04）

- [Salcara Desktop v1.6.1](https://github.com/dkdjndbfj-wq/salcara-desktop/releases/tag/v1.6.1) 已正式公开，标记为 Latest；不是草稿或预发行。发布目标与标签保持 `5e267fcb5adb0fee162ed96b98a9ac58764c178a`，没有替换标签或覆盖资产。
- 正式仓库只有 `main` 分支，产品介绍、下载入口和新用户教程已在公开首页展示；没有发布另一套桌面项目。本地有未提交内容的旧工作目录保留，不是此次构建或发布来源。
- 以下六个用户发布资产均为 uploaded，名称、大小、SHA256 元数据和校验清单一致（GitHub 自动生成的源码归档不在这六项中）：

| 文件 | 字节 | SHA256 |
| --- | ---: | --- |
| `Salcara-Desktop-1.6.1-win32-x64-setup.exe` | 142469699 | `da8191014c643398ccca038c67255dd14d18dc7a39996552a7aec0994f25c666` |
| `Salcara-Bridge-1.6.1-win32-x64.zip` | 205985948 | `fbbcb6a2c582e6f212a942d815bfebf32cf567feca8e2e1748f75b8c43832f18` |
| `Salcara-Bridge-1.6.1-win32-x64.tar.gz` | 199914876 | `b743f0d232187f99d563a9f216bd52524dcd97e7e5abfb4f337449b6d1ed3043` |
| `latest.json` | 2156 | `5fd55c5ffc77da42195e935f3c3922ddd1fd6712dfd3a72a8fb03fa3c039000b` |
| `latest.json.sig` | 89 | `2a10eb26c4f94f788e22efb2da275b4037a0ebb65cfec38358c59ac412381a72` |
| `SHA256SUMS.txt` | 478 | `9a750e520b81dde2cff9c4f5365ced355aad1393e8e97dba822b351314ad22cd` |

- 独立匿名网络验收通过：未使用 GitHub 登录、令牌、Cookie 或签名私钥；公开 Latest API 返回正确版本、源码身份和六资产清单；下载三个小文件并计算完整 SHA256，与公开资产元数据一致；五项校验清单与资产 digest 全部对应。
- 用应用附带的原公钥独立验证公开下载的 `latest.json.sig`，确认 1.6.0 客户端可验证 1.6.1 更新清单；签名 tar 的名称、大小与 SHA256 均一致。没有读取本地发布私钥。
- 匿名访问 [Windows 安装包](https://github.com/dkdjndbfj-wq/salcara-desktop/releases/download/v1.6.1/Salcara-Desktop-1.6.1-win32-x64-setup.exe)，Range `0-4095` 返回 206，文件总大小为 142469699 字节，MZ/PE 头验证通过。该项是公开可下载性及文件头抽查，不冒充本机再次下载整个安装包；完整安装包和 ZIP/tar 的哈希与内容由上述两轮 CI 验证。
- Node 直接 HTTPS 下载曾遇到连接重置/超时；改用禁用自定义配置的 Windows curl 匿名请求后通过。没有改全局代理、更新通道或发布文件。
- 最后核对：产品用户文档中没有“副本/交接/待发布安装包”等过程表述；桌面和手机 UI/动效未改，运行内核未改。仓库提交清单不包含用户 API、真实设备配置、服务器凭据或发布私钥。

已发布不代表所有设备能力均已实机验收：Windows Authenticode 证书仍未配置；CodeRabbit 因不支持本机系统而未出具审查结果；真实 Agent/手机全链路、真实 Electron A/B 自动更新和 Inno 中途复制失败恢复故障注入仍属于前述未实测边界。
