# 桌面自动更新

1.6.0 配置本仓库更新通道和独立 Ed25519 公钥。此前公钥 / 仓库为空的开发版本不能自动获得信任配置，首次需手动安装完整新包。

## 发布者流程

首次签名身份由 `scripts/release-keygen.cjs` 创建，私钥位于仓库外 `.salcara/signing/desktop/update-signing-key.pem`，不得提交。公钥固定进 `package.json`，后续更新不能随意换钥匙。更新身份不是 Windows Authenticode 证书或 macOS notarization。

GitHub repository secret：`SALCARA_DESKTOP_UPDATE_PRIVATE_KEY`。手动运行 `Build signed Windows desktop`，测试后打包 Bridge、Electron、Node、companion、图标和许可证，签发 tar / manifest / sig，附首次下载 ZIP 与 SHA-256 清单。检查 artifact 后发布到同源码提交的 `v<数字版本>` Release，保持所有签名文件同一个标签，不替换签名私钥，不将源码 ZIP 当安装包。

后续版本提高 `package.json` 与 lockfile 的三段数字版本（例如 1.6.1）。跨平台文件汇总清单须重新签名，不自行修改已签 manifest。新用户用 ZIP；已安装客户端定期检查，使用原有更新窗口确认下载与重启。更新不会擅自停止正在运行的任务。

## 安装保障和边界

- 固定发布仓库 / 标签 / 文件，签名、版本、平台、压缩包大小与 SHA-256 校验。
- 解压前 USTAR 全量检查、完整安装标记、链接目录拒绝、替换范围校验。
- 本机自有核心闲置且审批已处理才能安全退出；与新任务共用 admission 锁，附着核心不被结束。helper 启动和安装许可成功后才退出。
- 新版本核心健康启动后随机令牌确认，失败回退或保留恢复副本，不强杀仍活着的未确认新进程。
- 不可写目录、开发运行、旧散装包只能从发布页手动下载。
- Windows 合成替换/回滚与签名回归通过，真实 Electron A/B 升级还需验收。Mac/Linux 本次不提供安装包，未声称实机验证。

详细改动与准确验证：[PUBLISH-20261004.md](PUBLISH-20261004.md)。
