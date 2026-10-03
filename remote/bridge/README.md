# Salcara Bridge · 桌面核心

桌面软件的 Go 核心与 Electron 外壳。当前为开发 / 实验版，版本由 `desktop/package.json` 提供。

完整范围、开发命令和验收限制见 [独立桌面仓库说明](../../README.md)。

- `internal`：配置、启动、协议转换、会话、桌面适配与本地控制台。
- `desktop`：现有 Electron 窗口、悬浮球、托盘、动画、签名 updater 与打包工具。
- `../desktop-companion`：明确授权的 Codex Desktop 实验适配器，不能随意移动目录。
- `e2e`：需要额外 Hub 源码的跨组件测试；普通单元测试用 `go test -short ./...` 明确跳过。

本地功能不依赖手机或远程 Hub。远程服务另行部署，模型 Key 不用作 Hub 身份。源码不附带真实配置、Key、配对身份或更新私钥。

许可见 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。打包保留完整目录与许可证；旧 `install/` 脚本不代表本仓库已经发布正式安装包。
