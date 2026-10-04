# Salcara Desktop 1.6.1

新增 Windows x64 安装程序，并完善面向用户的产品说明与使用教程。桌面界面、文案、动画及 Agent 内核保持不变。

## 下载哪个文件

- **推荐：`Salcara-Desktop-1.6.1-win32-x64-setup.exe`**。双击安装，无需管理员权限，提供开始菜单/可选桌面快捷方式和 Windows 卸载入口。
- **便携版：`Salcara-Bridge-1.6.1-win32-x64.zip`**。解压完整目录后运行 `Salcara Bridge.exe`，不要单独移动 exe。
- `SHA256SUMS.txt` 是文件校验清单；`.tar.gz`、`latest.json` 和 `.sig` 用于应用自动更新。

## 本次改进

- 安装器与自动更新目录分离，后续更新不会替换或丢失卸载程序。
- 卸载保留本机 API、配对配置及第三方 Agent 的会话和项目；不会卸载 Codex 或 Claude。
- 拒绝用较旧安装包覆盖较新的已更新应用。
- README 改为产品介绍、下载入口和快速入门，详细操作与开发维护文档分别整理。

1.6.0 正式客户端可通过既有签名更新通道升级应用，但不会自动增加 Windows 卸载登记。想改用安装版，请结束任务并退出旧便携程序，再运行 setup；用户配置仍沿用原位置。

更新公钥不变。Ed25519 更新签名不是 Windows Authenticode 证书，SmartScreen 仍可能提示未知发行者。当前只提供 Windows x64 二进制；真实 Agent/手机端到端及真实 Electron A/B 自动更新仍需设备验收。远程能力和 Claude Desktop 普通 Chat / Cowork 限制未改变。

[使用教程](https://github.com/dkdjndbfj-wq/salcara-desktop/blob/main/docs/USER-GUIDE.zh.md) · [反馈问题](https://github.com/dkdjndbfj-wq/salcara-desktop/issues)
