# Salcara Desktop · 桌面 Agent 工作台

独立桌面软件源码：命名 API 密钥库、Agent 配置与启动、原会话管理，以及可选的手机远程连接。

Windows 完整桌面构建和签名更新流程已配置；可下载包请查看本仓库 Releases。开源和签名不代表原生桌面接口、自动更新和手机链路已经全部实机验收。

## 范围

- `remote/bridge`：Go 核心、本地控制台、Electron 桌面外壳。
- `remote/desktop-companion`：需要用户明确授权的 Codex Desktop 实验适配器。
- `assets/brand-logo.png`：桌面构建所需共享资源。

不包含手机 App、中转站插件、个人 Hub、真实 API / 配对数据、签名私钥，也不捆绑 Codex / Claude 本体。本地 API 与 Agent 功能不要求部署 Hub。

## 能力与边界

- 同一命名 API 可供不同 Agent 使用；实际模型与工具兼容性取决于上游协议及 Agent。
- 本地工具配置、启动、会话记录和本机用量；工具配置修改与重启需要用户在软件里明确操作。
- 原样保留当前固定窗口、悬浮球、托盘、启动动画和主题，不重新设计 UI。
- 手机远程需另行部署兼容 Hub；电脑保持在线，桌面原生操作还需要适配器授权。
- Codex Desktop 是实验适配，未知宿主或权限不足时拒绝；Claude Desktop 原 Chat / Cowork 直接发送、停止仍未实现，不能用 CLI 冒充。
- 1.6.0 配置本仓库更新通道和独立 Ed25519 公钥；更新签名、大小、SHA-256 和安装范围均需验证。任务未结束、非自有核心或不可写目录不会强制安装。

本地 Key / 配置备份并非完整系统钥匙串保护，勿共享真实配置。远程 Hub 当前不是端到端加密通道，中继所在站点可接触转发内容，只连接可信服务。

## 开发

使用 Go 1.27.1、Node.js 22 的当前维护版本或满足依赖 engines 的较新版本。桌面按构建机器自身的系统 / 架构打包。

```sh
cd remote/bridge/desktop
npm ci
npm run dev
# 只构建完整桌面目录，不发布正式更新
npm run package
```

`npm run dev` 会启动桌面软件并访问本机配置，实验验证建议采用独立测试配置。产物在 `remote/bridge/desktop/out`，必须保留完整目录，不能只移动 exe。Go 不在 PATH 时通过 `GO_BIN` 指定可执行文件。

源码测试：

```sh
node --test remote/bridge/desktop/test/*.test.cjs
node --test remote/desktop-companion/tests/*.test.mjs
cd remote/bridge
go test -short ./...
go vet ./...
```

`-short` 明确跳过需要独立 Hub 的跨组件 e2e；本仓库不复制服务器当桌面依赖。Windows 既有平台断言与准确测试结果见 [发布交接](docs/PUBLISH-20261004.md)。Actions 提供手动 Windows 签名打包入口，先上传 artifact 供验收，不自动覆盖 Release。

## 后续更新与许可

签名发布流程与更新边界见 [自动更新](docs/AUTO-UPDATE.md)。旧公钥 / 仓库为空的开发版本首次需手动安装新版本，不能自动获得信任身份。

沿用原源码的 [MIT 许可证](LICENSE) 和版权声明。第三方归属见 [通知](remote/bridge/THIRD-PARTY-NOTICES.md)；构建会收集 Go / Node 许可证，Electron 自带许可。实验适配器保留自己的 [MIT](remote/desktop-companion/LICENSE)。
