# 2026-10-04 Windows companion 私有权限发布修复

只修桌面内核和测试；没有修改手机 / 桌面 UI、样式、文案、动效，也未读取或改写真实 Codex / Claude 配置。所有回归使用临时合成文件和内存安全描述符；不启动任何真实 Agent、不打印令牌、不启用系统特权。

## 根因与范围

Bridge `setPrivatePermissions` 原先只写当前用户 + SYSTEM 的受保护 DACL，没有设置 owner。提升权限的 Windows runner 默认新文件 owner 可能是 Administrators，而读路径 `checkPrivatePermissions` 要求 owner 为实际运行用户 SID。因此安装产生的 root、payload、ownership state、active-desktop.json 可能因 owner 不一致被判定未激活，表现为 `desktop_activation_required`。备份权限测试之前又只请求 DACL，未检查实际 owner，并依赖 SDDL 文本中的完整 SID；Windows 的 SDDL 缩写会导致身份检查误判。

实际 companion 的 Node descriptor writer 已经在写入前调用 `SetOwner($sid)`；无需重写它或削弱 Go 读取端。微软说明新对象使用创建 token 的默认 owner，设置 owner 需要 `OWNER_SECURITY_INFORMATION` 与 owner SID，见 [Owner of a New Object](https://learn.microsoft.com/en-us/windows/win32/secauthz/owner-of-a-new-object) 和 [SetNamedSecurityInfoW](https://learn.microsoft.com/en-us/windows/win32/api/aclapi/nf-aclapi-setnamedsecurityinfow)。

## 文件修改

- `remote/bridge/internal/desktopcompanion/permissions_windows.go`：写入私有目录 / 文件时，在同一次 Windows 设置调用中添加 `OWNER_SECURITY_INFORMATION` 并传入当前 `TokenUser` SID。原受保护 DACL、当前用户与 SYSTEM 两个 Allow 授权、目录 OI/CI 继承范围保持不变。不把默认 Administrators 视为合格 owner、不授予管理员组额外访问、不启用 TakeOwnership / Restore privilege。
- 同文件的只读检查仍请求 owner + DACL、拒绝 owner 不一致、未保护或 ACE 数量不等于二的 DACL；精确检查真实 ACE SID，而不是 `sd.String()`。拒绝非 Allow、inherit-only、无效身份、第三主体及重复身份。不在读取授权令牌时自动修复 ACL。
- `remote/bridge/internal/desktopcompanion/permissions_windows_test.go`：备份 / ownership archive / config / private directories 同时读取并独立验证 owner 与真实 ACE。新增 `writeExclusive` 与 `atomicWrite` owner 一致性；合成 SID / SDDL alias 的稳定解析；外部 owner、未保护、过宽组、第三主体、Deny、inherit-only、重复主体拒绝；读取失败不改变权限。没有跳过测试。

## 已执行验证

- Windows 本机 `go test ./internal/desktopcompanion -count=1` 全包通过。
- Windows 本机 `go vet ./internal/desktopcompanion` 退出 0。
- 指定两个修改 Go 文件的 `git diff --check` 退出 0。
- 此本机成功不能替代提升权限 GitHub runner 的回归结果；需要最终 Windows CI 重跑，并继续构建完整桌面包。未 commit、push 或发布，交由主任务统一确认。

本次 owner 修正是安装器写入时兑现既有安全契约，不是让用户绕过原生桌面授权。已安装的 foreign / 管理员组 owner 描述符依然不会在读取时被自动修改或视为已授权。
