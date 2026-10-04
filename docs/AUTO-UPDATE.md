# 桌面发布与自动更新

用户更新操作见 [使用教程](USER-GUIDE.zh.md#7-更新)。本页面向发布维护者。

## 信任身份

自 1.6.0 起，正式客户端固定使用 `dkdjndbfj-wq/salcara-desktop` 与 package.json 中的 Ed25519 公钥；1.6.1 保持不变。未配置公钥/通道的早期开发版需手动首次安装。

发布私钥只在仓库外或 GitHub repository secret `SALCARA_DESKTOP_UPDATE_PRIVATE_KEY` 保存。不要运行 keygen 来替换已有身份，不要提交、打印或写入日志。更新签名不是 Windows Authenticode 证书或 macOS notarization。

## 发布步骤

1. 提高 `package.json` 和 lockfile 顶层版本，新增 `docs/RELEASE-<版本>.md`。版本必须为三个数字。
2. 手动运行 `Desktop source checks (manual)` 和 `Build Windows installer and signed updates`，使用同一精确源码提交。
3. Windows 流程执行桌面/companion/Go 测试、vet，构建完整 Electron 目录，编译 Inno Setup 安装器；在一次性用户中测试安装/重装/降级拒绝/卸载，再生成签名更新、便携 ZIP 和校验清单。
4. 成功后运行 `Verify prior Windows build and stage release`，填入原构建 run ID 与 40 位 source SHA。它只接受指定构建 workflow 的确切成功 run，下载该 artifact，以被测源码的公钥和规则验证，创建同提交的 draft。
5. 检查源码、资产、签名、平台和测试记录后公开 draft。不得覆盖已有标签、使用 `--clobber` 或重新签发同版本资产。

一份 Windows x64 Release 必须有以下六个文件：

- `Salcara-Desktop-<版本>-win32-x64-setup.exe`：首次安装。
- `Salcara-Bridge-<版本>-win32-x64.zip`：便携下载。
- `Salcara-Bridge-<版本>-win32-x64.tar.gz`：应用更新载荷。
- `latest.json` 与 `latest.json.sig`：同一版本的更新清单与签名。
- `SHA256SUMS.txt`：包含其余五个文件的 SHA-256。

setup 和 ZIP 的哈希列在校验清单中；签名 manifest 指向 tar 更新载荷，不承诺 setup 已有 Windows 代码签名。GitHub 自动生成的 Source code 不是安装包。

## 安装布局与更新兼容

```text
%LOCALAPPDATA%\Programs\Salcara Desktop\
  unins000.exe / unins000.dat
  .salcara-installer
  app\
    Salcara Bridge.exe
    .salcara-install.json
    resources\
```

Inno Setup 仅为当前用户安装，payload 在独立 `app` 子目录。既有 updater 按 exe 所在目录整体替换，因此卸载器必须留在外层。配置和 Agent 记录沿用原 AppData/工具目录，安装与卸载不迁移或删除。

1.6.0 便携用户按原协议升级应用，不会自动得到 Windows 卸载条目；想改为安装版需手动运行 setup。安装器校验产品范围和版本，拒绝覆盖更高版本，不强行关闭任务。

## 自动更新保障和边界

- 固定仓库、标签、文件名；验证 Ed25519、版本、平台、实际下载大小与 SHA-256。
- 解压前全量检查 USTAR，拒绝越界、链接、设备、重复/冲突路径及异常大小。
- 本机自有核心空闲且审批已处理才能退出，与新任务共用 admission 锁；附着的非自有核心不被结束。
- helper 和安装许可准备好后才退出；新版本健康启动后随机令牌确认，失败回退或保留恢复备份，不强杀仍活着的未确认进程。
- 不扫描删除任意历史 sibling 目录；开发运行、不可写目录或无法验证的散装包转为手动下载。

Windows 安装器实测、合成目录替换和签名检查必须分别记录，不用 CI 打包成功冒充真实 Electron A/B 或手机链路验收。Mac/Linux 当前未提供正式安装包。历史 1.6.0 验证见 [维护记录](maintainers/RELEASE-1.6.0-VERIFICATION.md)，本轮改动见 [1.6.1 维护记录](maintainers/RELEASE-1.6.1-REVIEW.md)。
