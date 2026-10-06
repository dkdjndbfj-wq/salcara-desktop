# Desktop 1.6.2 发布验收

- 被测源码：`047b3ace0fa1873dbed621fcc6fa20617c7d79ec`。
- Linux source-check：`37472122297`，Node、companion、Go unit/vet/race 成功。
- Windows build：`37472130276`，完整 Electron、当前用户 Inno 安装器、真实隔离用户安装/重装/降级拒绝/卸载成功。
- 独立核验与 draft：`37473155476`，使用原被测源码、公钥及确切 build artifact；不是重新构建的替代文件。
- 2026-10-06 公开 [v1.6.2](https://github.com/dkdjndbfj-wq/salcara-desktop/releases/tag/v1.6.2)，六份正式资产齐全。
- 本地再次下载全部 Release 文件，SHA256SUMS、Ed25519、完整 USTAR、安全路径、安装器 PE 及便携包完整性通过。更新 tar SHA-256：`761e1029c134d90f69a43b13be62d9dbbea72933f0e5976665fada1f0f795018`。
- setup SHA-256：`4dc75bc79c08cee49d8e3a46488ba041ed8c78e8c249ef737bffeead8efe6b35`。
- 保留原更新公钥及仓库通道，公开 Release 成为 latest 后旧正式客户端可发现更新。

本轮源码与公开说明通过敏感信息扫描，新提交使用 GitHub noreply 邮箱；未提交真实设备配置、API Key、服务器凭证或签名私钥。没有 Windows Authenticode 证书，不将更新签名描述成系统信任签名。

窗口按钮与原生文件夹选择的真实用户桌面验收、Android + Windows + 双公网 Hub 弱网验收仍未完成。交接前 A Hub 完全不可达时没有独立备用控制通道。发布包和自动化检查成功不能代替上述设备端验收；能力边界见版本说明。
