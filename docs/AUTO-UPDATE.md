# 桌面自动更新准备

先开源源码，更新通道保持关闭。本次未生成发布签名私钥、上传安装包或创建签名 Release。

## 已有代码

以下路径相对 `remote/bridge/desktop`：

- `updater.cjs`：版本 / Ed25519 签名、平台、大小及 SHA-256 校验，目录替换和回退逻辑。
- `scripts/release-keygen.cjs`：发布者在项目外生成自己的更新私钥。
- `scripts/release.cjs`：由已打包正式版生成 archive、`latest.json` 和 `latest.json.sig`。
- `package.json` 的 `salcaraUpdateRepo` / `salcaraUpdatePublicKey` 保持为空，开源不会突然向用户推送更新。

当前仍为 `1.5.1-test.2`，现有工具拒绝把测试版当正式更新发布。
当前比较器按三段数字比较版本，不把去掉 `-test.2` 当作升级；首个更新还需提高数字版本，不能直接发布同数字的 `1.5.1` 就期待自动更新。

## 下一阶段

1. 验证真实 Windows 打包、进程关闭 / 重启、目录锁定和回退，确认各平台支持范围。
2. 建立发布签名身份，私钥不进仓库；仓库 / 客户端只放公钥。更新密钥与 Windows Authenticode / macOS notarization 不是同一身份。
3. 配置仓库 `dkdjndbfj-wq/salcara-desktop` 和已验证公钥，编入客户端。旧空配置版本不会自动获得这些设置。
4. 从审查过的源码构建正式版，生成平台 archive 和签名 manifest；多平台条目汇总后必须重新签名。
5. 先验证坏签名、错误 hash / 平台、降级、中断下载、替换失败和正在执行任务时的处理，再正式启用。

目前验签单测不等于完整安装链已经安全验收。不要关闭签名检查，也不要把 GitHub 自动生成的源码压缩包当 updater 安装包。
