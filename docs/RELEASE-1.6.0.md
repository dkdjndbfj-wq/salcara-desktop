# Salcara Desktop 1.6.0

Windows x64 完整桌面程序。解压 ZIP 后运行文件夹内的 `Salcara Bridge.exe`，不要单独移动 exe。用户配置和会话不放在安装目录里。

- 保留 Claude 完成的桌面界面、动效、Agent / API 工作台与会话功能。
- 启用本仓库的签名自动更新：清单 Ed25519 签名、压缩包大小及 SHA-256 验证。
- 更新安装前等待任务结束并安全退出自有核心，不强制结束正在运行的 Agent；借用的核心不会被更新程序结束。
- 安装失败回退；新程序只有核心正常启动后才确认成功，无法确认时保留恢复副本。

`latest.json`、`latest.json.sig`、`.tar.gz` 是自动更新文件，ZIP 用于首次安装。更新签名不等同于 Windows Authenticode 证书，首次运行仍可能出现 SmartScreen 提示。

Codex Desktop 实验适配仍需用户授权；Claude Desktop 原 Chat / Cowork 直接发送或停止尚无支持，不以 CLI 冒充。手机远程依赖兼容 Hub、电脑在线和工具权限。尚未完成全部真实设备端到端验收。
