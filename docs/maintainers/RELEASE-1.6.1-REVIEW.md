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
- `scripts/test-installer.ps1`：仅 GitHub Windows runner，一次性用户/profile 的真实安装器回归，不启动 Agent/App、不触碰开发者配置。
- `test/installer.test.cjs`：合成载荷/调用边界测试；`update-hardening.test.cjs` 增加安装版 parent/app 布局，确认更新/回滚不删除外层卸载器。
- `package.json` 与 lockfile：1.6.1，加 `installer` 命令，依赖和签名公钥不变。
- `desktop-release.yml`：安装器构建及独立用户 smoke，六文件统一校验，签名 secret 只导入临时签名步骤。
- `verify-release-artifact.cjs`：资产清单从五个增至六个，setup SHA256 与 Windows PE 验证；tar 签名协议不变。
- `desktop-publish-verified.yml`：使用确切被测源码自身的 verifier，不再从当前 main 替换；草稿显示 Salcara Desktop 产品名。

## 已验证（本机）

- Windows desktop Node：42 项，41 pass，0 fail，1 POSIX e2e skip；含完整合成 Windows swap/rollback，外层卸载器保留。
- Windows companion：93 pass，0 fail/skip。
- 本机未安装 ISCC，真实 setup 编译/安装器执行由 Windows CI 验证，未在本机安装覆盖正式 App。
- CodeRabbit 审查技能：CLI 缺失后尝试官方安装命令，返回 `Unsupported operating system: mingw64_nt-10.0-26200`。没有启动审查、没有结果，不以手工审查替代或声称其通过；需要受支持的 Linux/macOS 环境安装、登录后才能补做。

## 发布门槛与边界

发布前须完成真实 Inno 编译及隔离用户安装/重装/降级拒绝/锁定与链接拒绝/卸载/配置和恢复数据保留测试，Linux source-check、六个资产的 SHA256、Ed25519 更新签名和完整 ZIP 检查，并核对 UI/内核 diff 和敏感文件清单。

不能把安装器 smoke、合成进程或 CI build 当真实 Codex/Claude/手机全链路或真实 Electron A/B 更新验收。setup 没有 Windows Authenticode 签名；此版本不声称消除了全部隐藏问题。

CI 与正式资产结果在实际完成后追加。
