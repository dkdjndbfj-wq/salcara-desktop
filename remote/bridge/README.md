# Salcara Desktop · 桌面核心

这里是 Salcara Desktop 的 Go 核心和 Electron 外壳。产品介绍、下载入口与快速入门见 [项目首页](../../README.md)，操作教程见 [使用教程](../../docs/USER-GUIDE.zh.md)，构建说明见 [开发指南](../../docs/DEVELOPMENT.md)。

- `internal`：配置、启动、协议转换、会话、桌面适配与本地控制台。
- `desktop`：Electron、签名更新、完整应用打包和 Windows 安装器。
- `../desktop-companion`：既有实验桌面适配组件的兼容实现。
- `e2e`：需独立 Hub 的跨组件测试，普通单元测试使用 `go test -short ./...`。

本地功能不依赖手机或 Hub。远程服务独立部署，模型 API Key 不用作 Hub 管理身份。源码不附带真实配置、Key、配对身份或发布私钥。

许可见 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。`install/` 中的历史服务端模板不是当前桌面安装方法；用户应从正式 Releases 下载完整安装包。
