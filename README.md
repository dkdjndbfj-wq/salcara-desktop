# Salcara Desktop

开源的桌面 Agent 工作台。在一个地方管理多组 API，为 Codex、Claude Code 和 Claude Desktop 选择服务商、配置并打开原工具；需要时，也可以配合 Salcara 手机 App 和 Hub 远程继续编程。

本地使用不需要注册 Salcara 账号，也不需要部署服务器。API 可以自行命名，同一组 API 可以供多个 Agent 使用，不绑定某个品牌。

[下载 Windows 安装包](https://github.com/dkdjndbfj-wq/salcara-desktop/releases/download/v1.6.3/Salcara-Desktop-1.6.3-win32-x64-setup.exe) · [全部版本](https://github.com/dkdjndbfj-wq/salcara-desktop/releases) · [使用教程](docs/USER-GUIDE.zh.md) · [反馈问题](https://github.com/dkdjndbfj-wq/salcara-desktop/issues)

## 可以做什么

- **管理 API**：保存服务商地址、密钥和名称，在不同 Agent 之间共享选择。
- **配置与启动 Agent**：为已安装的工具应用 API 配置，打开或重启原应用；模型在 Agent 自己的菜单里选择。
- **读取和恢复编程会话**：查看本机支持的会话记录，继续已有项目；不因换 API 新建一套 Codex 用户目录。
- **跨协议模型接入**：本地网关转换受支持的 Responses、Chat Completions 和 Messages 请求。能否正常使用仍取决于服务商接口、模型工具调用能力和 Agent 版本。
- **可选手机远程**：通过自行部署或可信站点提供的 Salcara Hub 扫码配对，查看支持的编程会话并发送任务。
- **签名自动更新**：检查新版本，验证更新签名及文件完整性，在任务结束后确认安装。

## 下载与安装

当前提供 **Windows 10/11 x64** 桌面包。Codex、Claude Code、Claude Desktop 需要另外安装，Salcara 不捆绑这些工具。

| 下载文件 | 用途 |
| --- | --- |
| `Salcara-Desktop-1.6.3-win32-x64-setup.exe` | 推荐。安装向导、开始菜单快捷方式、Windows 卸载入口 |
| `Salcara-Bridge-1.6.3-win32-x64.zip` | 免安装便携版，解压完整文件夹后运行 |
| `SHA256SUMS.txt` | 下载文件的 SHA-256 校验清单 |

双击安装包，按向导安装到当前用户目录，然后从开始菜单打开 **Salcara Desktop**。无需管理员权限。为了兼容已有配置，应用窗口和可执行文件仍使用 `Salcara Bridge` 名称。

便携版请运行解压目录中的 `Salcara Bridge.exe`，不要只复制这个 exe。GitHub 自动生成的 **Source code** 不是可直接运行的安装包；`.tar.gz`、`latest.json` 和 `.sig` 是客户端自动更新文件。

目前没有 Windows Authenticode 代码签名证书，SmartScreen 可能提示未知发行者。更新文件的 Ed25519 签名不等于 Windows 系统信任证书。请只从本仓库 Releases 下载，不要关闭系统安全功能。

## 第一次使用

1. 打开 **API 密钥**，添加 API 名称、服务商地址和密钥。
2. 打开 **Agent**，在工具卡片里选择这组 API。工具未安装时，可从 **环境** 页面查看安装入口。
3. 点击 **打开**，加载服务商模型；需要时开启模型菜单覆盖。
4. 确认当前任务已结束，再配置并打开原工具。进入工具后选择模型，开始或继续项目。

只在电脑上使用，到这里就够了。换 API、恢复原配置、Claude Desktop 第三方模式及手机配对的具体步骤见 [完整使用教程](docs/USER-GUIDE.zh.md)。

## 手机远程（可选）

桌面 App 负责访问这台电脑的工具和项目，手机 App 负责移动端操作，Hub 负责设备配对和消息中继。三者是独立项目，不需要把服务器安装到桌面 App 里。

- [Salcara 手机 App](https://github.com/dkdjndbfj-wq/salcara-image-mobile)
- [Salcara Hub：Docker 部署与管理](https://github.com/dkdjndbfj-wq/salcara-hub-plugin)

在 **手机远程** 中连接已经部署 Hub 的站点，显示二维码，再用手机扫码绑定。电脑必须在线且未休眠；只连接可信的 Hub，当前中继不是端到端加密，Hub 运营者可能接触会话内容。

远程编程不是整台电脑的屏幕投影或通用远程桌面：Codex 会话可通过支持的编程后端继续，Claude Desktop 的 Code 会话通过 Claude Code 继续。**Claude Desktop 普通 Chat / Cowork 不支持直接远程发送或停止**；桌面窗口也不保证立即刷新手机续聊内容。详细限制见教程，不将 CLI 能力冒充桌面原生能力。

## 更新与卸载

现有正式客户端使用同一更新通道和公钥。1.6.3 起每 2 小时检查，提示和下载不会停止任务或自行安装；任务结束后再主动确认重启安装。早期未配置更新通道的开发版需手动安装正式包。

1.6.1 / 1.6.2 内置更新失败时，请退出 Salcara 后手动运行 1.6.3 setup 一次，无需先卸载或删除 AppData。新版安装器兼容旧卸载留下的更新缓存，保留配置和会话；不要继续反复尝试旧更新器。

安装版可在 Windows **设置 → 应用 → 已安装的应用 → Salcara Desktop** 卸载。卸载保留 API 配置、配对数据及第三方 Agent 的项目和会话；重装可继续使用。请妥善保护本机配置，它包含敏感密钥，不是完整的系统钥匙串保险库。

## 开发与贡献

这是完整的桌面项目，正式源码统一维护在 `main`。开发、测试、安装器构建和签名发布步骤见 [开发指南](docs/DEVELOPMENT.md) 与 [自动更新](docs/AUTO-UPDATE.md)。欢迎提交 Issue 和 Pull Request；请勿附带 API Key、配对令牌或真实用户配置。

## 许可

使用 [MIT 许可证](LICENSE)。第三方依赖与版权声明见 [第三方通知](remote/bridge/THIRD-PARTY-NOTICES.md)，桌面适配组件保留其 [MIT 许可证](remote/desktop-companion/LICENSE)。Codex、Claude 等品牌及工具属于各自权利人；Salcara 是独立项目，不代表其官方产品。
