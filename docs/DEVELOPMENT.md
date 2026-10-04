# 开发与构建

用户安装与操作请先看 [使用教程](USER-GUIDE.zh.md)。本页面向贡献者和发布维护者。

## 源码结构

- `remote/bridge`：Go 核心、本地控制台、工具配置、会话读取和本地网关。
- `remote/bridge/desktop`：Electron 外壳、签名更新器、打包与 Windows 安装器。
- `remote/desktop-companion`：兼容既有安装的实验桌面适配组件；不是当前普通用户的安装前置条件。
- `assets`：构建所需品牌资源。

本仓库不含手机 App、Hub 服务、第三方 Agent 本体、用户配置、配对身份或发布私钥。旧 `remote/bridge/install` 中的服务端模板不是当前桌面安装方法，不要用它代替 Releases 安装包。

## 环境与开发

使用 Node.js 22（至少 22.12）和 Go 1.27.1。Go 不在 PATH 时，以 `GO_BIN` 指定工具路径。构建针对构建机器自身的系统与架构；当前正式二进制仅发布 Windows x64。

```sh
cd remote/bridge/desktop
npm ci
npm run dev
```

`dev` 会启动 App 并访问本机配置。调试时建议先设置绝对路径 `SALCARA_BRIDGE_CONFIG_DIR` 指向专用测试目录，并设置独立 `SALCARA_BRIDGE_PORT`，不要用真实 API 或原工具任务进行合成回归。

## 测试

```sh
node --test remote/bridge/desktop/test/*.test.cjs
node --test remote/desktop-companion/tests/*.test.mjs
cd remote/bridge
go test -short ./... -count=1
go vet ./...
go test -short -race ./... -count=1
```

PowerShell 中可先用 `Get-ChildItem -File -Filter '*.test.cjs'` 列出 Node 测试，再将路径数组传给 `node --test`。 `-short` 跳过依赖独立 Hub 的跨组件 e2e；合成宿主或目录更新测试不等于真实手机/桌面完整链路验收。

Windows CI 排除五项 POSIX/macOS 专属历史断言，Linux source-check 不排除并执行 race。具体排除正则和准确命令保留在 `.github/workflows/desktop-release.yml`。

## 打包

```sh
cd remote/bridge/desktop
npm ci
npm run package
npm run installer
```

`package` 输出完整 Electron 应用目录到 `out`。Windows `installer` 需要 Inno Setup 6，自动定位标准 ISCC 路径，也可用 `SALCARA_ISCC` 指定编译器；它包装已构建的完整目录，不修改 App UI。当前用户安装目录固定为 `%LOCALAPPDATA%\Programs\Salcara Desktop`，可更新应用位于 `app` 子目录，卸载器在外层。

真实安装器 smoke 仅允许在 GitHub Windows runner 运行，使用一次性本地用户与独立 profile，不启动真实 App。详见 `scripts/test-installer.ps1`。开发者本机不要为测试安装覆盖自己的正式软件。

## 发布

发布步骤、长期公钥及验收要求见 [AUTO-UPDATE.md](AUTO-UPDATE.md)。CI 只构建或生成经过验证的草稿；维护者确认后才公开 Release。不要覆盖已有标签或签名资产。发布私钥必须保存在仓库外或 GitHub Secret，不能提交。

本轮安装器与文档改动记录：[1.6.1 维护记录](maintainers/RELEASE-1.6.1-REVIEW.md)。
