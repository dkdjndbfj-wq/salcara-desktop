# Desktop 1.6.3 安装与更新修复记录

## 范围

仅修改 Windows 安装/卸载、桌面更新内核、窗口事件处理和回归测试。未修改手机、Hub、Agent 配置、桌面页面布局、配色、既有文案或动效；仅新增必要的更新异常提示，既有签名公钥与更新仓库不变。

## 发现与修复

1. **1.6.1 / 1.6.2 卸载后重装被拒绝。** 更新 staging 被保留，Inno 却删除根目录身份，重装见到“非空且不属于安装器”。新安装保留身份；兼容旧版无身份但只剩非冲突 recovery 目录的情况，不读取、删除或覆盖其内容。未知文件、已有未知 app、链接及错误身份仍拒绝。
2. **更新进程启动不等于脚本执行。** Windows 原启动选项在复现环境中可返回 exit 0 而没有执行 PowerShell；非 detached 子进程又会进入 Node 的退出清理 job。使用 bundled Node 的独立监督进程，运行时复制到 AppData 的专用随机目录，不锁安装 payload，监督非 detached PowerShell 完成更新。
3. **无交接确认即退出。** 提交核心退出前验证随机令牌就绪文件，失败取消许可、恢复 admission，不退出原程序。helper 全过程在安装树之外工作；新进程确认健康才移除精确旧备份，失败回退，不强杀仍活着的未确认新进程。
4. **旧 staging 不能按名字认领并删除。** staging 增加来源标记，未知缓存只保留改名；自动清理仅处理本程序有标记的缓存。监督运行时仅在 PID 不再存活且目录身份、固定文件清单都符合时清理，不扫历史 recovery。
5. **每两小时检查。** 自动弹窗采用 showInactive，不抢焦点，不触发准备/提交/退出。下载也是被动准备；只有用户主动安装才经过已有任务与审批检查。
6. **弹窗关闭范围。** 1.6.2 修复了主窗口事件和拖拽区域，未独立覆盖整个弹窗链路。本轮单独验证真实 update.html 按钮 → preload → IPC 的关闭/Escape 路径；通用关闭也只关闭自己的更新窗口，限制 sender、top frame 与准确 file URL。补充 Agent/API 小弹窗的实际事件委派、关闭后的迟到模型响应，以及所有页面面板关闭/重开与原动画计时器的回归测试；没有改动这些页面的生产前端。

涉及文件：`desktop/updater.cjs`、`desktop/update-ipc.cjs`、`desktop/window-ipc.cjs`、`desktop/main.cjs`、`desktop/installer/windows.iss`、`desktop/scripts/test-installer.ps1`、新增更新生命周期/弹窗测试、版本与本记录。

## 验证门禁

- 本地完整桌面 Node tests：最终修复源码已通过 66 项，65 passed / 1 skipped / 0 failed；唯一跳过为 Windows 上不适用的既有 POSIX E2E。
- Windows 完整 updater 合成宿主：真实签名/下载/USTAR/解压、从 app working directory 运行、原宿主退出、监督进程存活、健康重启与失败回退。
- 已有 Go 更新保护测试：`go test ./internal/hubclient ./internal/console -run 'Test.*Update' -count=1` 两包通过；覆盖 busy Agent、原生桌面 running、待审批、lease 过期、关闭 admission，不发模型请求、不停止任务。
- Windows 隔离用户安装测试：真实 installer 安装、重装、锁与链接拒绝、降级保护、卸载、保留 staging/recovery/user files 后直接重装、模拟旧版无 marker 的恢复。
- Linux / Windows CI、完整包与旧公钥签名核验完成后才公开新版本；不能替换 v1.6.2 的既有资产。

下列构建与发布验收已完成。合成宿主测试不代表用户真实桌面鼠标命中或公网远程弱网验收；不承诺所有 Windows 安全软件、环境或外部任务竞态无误。

本轮完整重跑曾发现 Windows 合成宿主测试的收尾竞态：健康日志早于 helper 删除精确旧备份，测试过早断言“备份已消失”。现等待记录中的监督进程结束后再断言，保留原备份清理断言与超时，不通过跳过或放宽业务断言绕过失败。`git diff --check` 与安装烟测 PowerShell parser 通过；没有修改 Agent 页面布局快照。

## 用户恢复与旧版本

1.6.2 仍含相同旧更新逻辑，下载它不能消除上述缺陷。旧版内置更新发生失败的用户应手动运行 1.6.3 setup 一次，不删除 API/配对配置或 Agent 会话。旧缓存保留以便恢复；安装器不强行结束任何进程。

本记录不包含真实配置、密钥、更新随机令牌或私人安装路径。实现使用 Inno 的 [uninsneveruninstall](https://jrsoftware.org/ishelp/topic_filessection.htm) 和隔离的 [child process](https://nodejs.org/api/child_process.html) 生命周期；实际行为以相应测试为准。

## 最终发布验收（2026-10-07）

- 被测源码：`9c0605d82d10b9e18d165d9ae0d61df9e1091ab6`；仅新建版本，不覆盖 1.6.2 标签或资产。
- Linux source-check：[37493426135](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/37493426135) 成功，含 Node、companion、Go unit/vet/race。
- Windows build：[37493435182](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/37493435182) 成功；包含完整 updater 成功交接/失败回退、Inno 编译、一次性用户安装/重装/降级拒绝/卸载、保留残留直接重装与模拟旧版无身份恢复。
- 独立核验/draft：[37494456591](https://github.com/dkdjndbfj-wq/salcara-desktop/actions/runs/37494456591) 成功，使用上述精确被测源码与原 build artifact，而非重构建替代文件。
- 六份 Release 资产已下载到仓库外，全部 checksum、原公钥 Ed25519、完整 USTAR、安全路径、setup PE 和便携目录再次本地核验通过。
- 更新 tar：199,978,224 bytes，SHA-256 `d99ecd1c3d3bcbcf95f7ed9561e73988c141031cdfa011ba1a98a77cf371c729`。
- setup：142,525,478 bytes，SHA-256 `3623f5968d53de291e82800254374663a02ee7203c66ea3a9d3c0910d6aef698`。
- [v1.6.3](https://github.com/dkdjndbfj-wq/salcara-desktop/releases/tag/v1.6.3) 已公开为正式 latest。公开后再次下载三份元数据，确认与独立核验文件逐字相同；公开资产 size/digest 和被核验的安装包/更新包一致。
- 源码提交使用 GitHub noreply；未上传本机诊断、API/配对配置、服务器凭据或签名私钥。没有 Authenticode 证书，不将更新签名描述为系统信任签名。
