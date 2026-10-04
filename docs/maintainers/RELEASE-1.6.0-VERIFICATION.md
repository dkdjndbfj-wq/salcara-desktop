# Salcara Desktop 1.6.0 验证记录

本页记录历史版本的发布证据，不是当前用户安装教程。用户请查看 [项目首页](../../README.md) 与 [使用教程](../USER-GUIDE.zh.md)。

## 版本与范围

正式应用源码：`8f690b261958f3f40c0ead8d4f523fd0e64df14d`。更新仓库为 `dkdjndbfj-wq/salcara-desktop`；独立 Ed25519 公钥自此版本写入 package.json，私钥仅在仓库外及 GitHub Secret 保存。

1.6.0 修复更新清单身份、流式下载上限、归档路径安全、取消清理、核心 prepare/commit/cancel 退出事务、安装许可、健康确认与失败回滚。Windows companion 的私有文件所有者明确为当前用户，受保护 DACL 保留当前用户及 SYSTEM，读取不会放宽权限。界面、图标及动效未改。

## 已验证

- Windows 本机 desktop Node：38 项，37 pass，0 fail，1 POSIX e2e skip；包含隐藏 PowerShell 合成成功替换、健康确认、启动失败回滚与取消。
- Windows companion：93 pass，无失败或跳过，合成授权宿主。
- Windows Bridge：短测试排除五项历史 POSIX/macOS 断言后通过，vet 通过；这些断言由 Linux 执行。
- Linux GitHub source-check `37139537723` 成功，含 POSIX 单元、companion、vet、race；跨组件 Hub e2e 使用 short 明确跳过。
- Windows GitHub build `37141888736` 完整成功：测试、vet、Electron/Bridge/stdio Node/资源/许可证打包、签名 tar、便携 ZIP。
- Artifact ID `11280333827`，GitHub ZIP digest：`sha256:3973777146cd0f64fcd96292792821a09ae753dd7edaf710b7d808a3f4d0f049`。
- 验收 run `37185060057` 下载上述确切产物，使用被测源码公钥与运行时策略验证 SHA256SUMS、Ed25519、USTAR、产品/版本/大小、安装标记及完整便携文件，然后创建草稿。辅助提交 `0a4dc09025e0a0571108c8cd086f6e7a52c4abea` 不重建应用。

## 正式资产

`v1.6.0` 已公开，标签和 target 指向上述应用源码。

| 文件 | 字节 | SHA-256 |
| --- | ---: | --- |
| `Salcara-Bridge-1.6.0-win32-x64.zip` | 205961319 | `3ef5e3746878726569fc46109441f916e4c8944771e1d1883ad87c366c068499` |
| `Salcara-Bridge-1.6.0-win32-x64.tar.gz` | 199887122 | `9dfc42a7f190564846005fd8dfa69982fbd67753b13ee325d49eaaaaa634f3a6` |

另发布 `latest.json`、`.sig` 和 `SHA256SUMS.txt`。此版本仅提供便携 ZIP，没有 setup 安装器；1.6.1 才增加安装器。

## 边界

构建及合成测试不等于真实 Electron A/B 升级或全部手机/Agent 端到端验收。没有 Windows Authenticode 证书，Mac/Linux 不在二进制发布范围。原生 Agent 能力以当前使用教程为准，旧实验桌面适配不是普通用户安装前置条件。

历史实现细节另见 [Windows 私有文件权限记录](../WINDOWS-COMPANION-PERMISSIONS-20261004.md)。本轮整理只重写公开文档，不改 1.6.0 的标签或资产。
