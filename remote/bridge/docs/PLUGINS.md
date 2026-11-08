# 本地插件

插件页管理安装在当前电脑上的扩展。安装入口接受包含 `manifest.json` 与 JavaScript 入口的文件夹；不需要改写 Salcara 的前端源码。示例位于 `remote/bridge/desktop/examples/activity-monitor`，安装后可以打开「工作动态」、点击「记录一次」，再从悬浮球的插件快捷入口打开同一个面板。

## 用户操作

1. 打开顶部「插件」→「安装插件」，选择插件文件夹。
2. 检查名称、作者、版本、说明和声明权限，再确认安装。取消不会安装。确认期间文件内容发生变化会拒绝安装，需重新选择和确认。
3. 插件卡片显示真实启动状态；可打开其面板、切换启停，或卸载。
4. 停用会停止该插件的线程、定时器、拦截器、事件订阅，并移除面板和快捷入口；插件文件与保存的数据保留。
5. 完全退出软件会清理运行实例，但不改变启用偏好。下次启动重新加载已启用插件，面板/快捷入口自动恢复。关闭主窗口到后台不会停止插件。
6. 卸载删除插件文件和该插件本地存储。删除失败会显示错误并保留可重试的记录；不会显示虚假的卸载成功。

插件代码须来自信任的本地来源。独立 Worker 用于避免插件卡住桌面、清理运行资源；它不是用于执行敌意代码的操作系统安全沙箱。没有在线插件市场、ZIP 解包或自动更新服务，插件包由用户选择本地文件夹安装。

## 清单

```json
{
  "id": "my-helper",
  "name": "我的助手",
  "version": "1.0.0",
  "minBridgeVersion": "1.6.4",
  "entry": "index.js",
  "author": "作者",
  "description": "插件说明",
  "permissions": ["ui.panel", "ui.satellite", "storage", "events"]
}
```

`id`、面板 ID、操作 ID、快捷入口 ID 使用 1–64 位字母、数字、下划线或连字符。入口必须位于插件目录内，安装拒绝符号链接和越界路径。最低版本在执行代码前检查。

| 权限 | 能力 |
| --- | --- |
| `ui.panel` | 注册纯文本面板和带回调的操作按钮 |
| `ui.satellite` | 注册悬浮球插件快捷入口 |
| `storage` | 当前插件独立的持久化键值存储 |
| `events` | 会话生命周期及工具检测事件 |
| `api.intercept` | 本机对话 API 提示词/JSON 回答处理，以及流式/用量观察 |
| `notify` | 系统通知 |

## 入口与界面

```js
exports.activate = async salcara => {
  let count = Number(await salcara.storage.get('count')) || 0;
  const panel = salcara.ui.registerPanel('main', {
    label: '我的助手', title: '我的助手', content: `已记录 ${count} 次`,
    actions: [{ id: 'record', label: '记录一次', onClick: async () => {
      await salcara.storage.set('count', count + 1);
      count++;
      panel.update({ content: `已记录 ${count} 次` });
      salcara.ui.toast('已保存', { type: 'ok' });
    } }],
  });
  salcara.ui.addSatellite('main', { label: '我的助手', panelId: 'main' });
};
exports.deactivate = async () => { /* 可选的协作清理；超时仍会结束线程 */ };
```

面板支持 `label/icon/title/content/actions`。`content` 按纯文本呈现，不能注入页面 HTML 或脚本。`actions` 每项包含唯一 `id`、`label` 与 `onClick` 函数；函数留在插件线程，主页面只接收文字和 ID。返回的句柄有 `open()`、`close()`、`update(opts)`；只更新 `content` 时保留已有按钮。

`addSatellite(id, {label, icon, panelId, onClick})` 可指定一个面板，或提供 `onClick` 回调。悬浮球将所有插件快捷入口收在可滚动菜单中，不无限扩张圆环。主窗口重载或悬浮球稍后创建时，会重新读取注册快照。

`ui.toast(message,{type:'ok'|'bad'})` 显示主窗口提示；`notify.send(title,body)` 需要 `notify` 权限，使用系统通知。

## 存储与恢复

`storage.get/set/delete/clear` 都返回 Promise。写入采用临时文件再原子替换，写失败会拒绝 Promise。每个插件最多 1 MiB、1024 个键，值必须能转换成 JSON。插件数据位于当前桌面配置目录的 `plugin-storage/<id>.json`；停用或退出保留，卸载删除。

不要将仅在内存里的任务状态当作已持久保存。进程被强制结束时不会调用 `deactivate`，已完成原子写入的数据可在下一次 `activate` 中读取。

## 对话 API 与事件

```js
salcara.api.intercept('request', async (request, next) => {
  // request = {tool, body, headers}; headers 不包含认证凭据。
  return next({ ...request, body: { ...request.body,
    instructions: (request.body.instructions || '') + '\n回答请简洁。'
  }});
});
salcara.api.intercept('response', async (response, next, request) => next(response));
salcara.api.on('stream', event => { /* 最佳努力观察流式 JSON 事件 */ });
salcara.api.on('usage', event => { /* 真实上游用量统计 */ });
salcara.events.on('session:update', event => { /* 生命周期元数据 */ });
```

请求钩子在本机 Codex/Claude 网关及命名 API 的授权检查完成后调用。允许改写 `instructions/input/messages/system`，以及合法 `X-Salcara-Plugin-*` 自定义头。账号、模型、工具、预算、会话标识、流式模式和认证信息不会由插件覆盖。远程账户网关不把内容送入本机插件。

响应钩子处理最终 JSON 回答；SSE/WebSocket 实时数据保持原样转发，通过 `stream` 事件观察，不能逐 token 改写。图片生成接口的图片字节不参与拦截。`usage` 采用真实上游统计，不受改写回答影响。

会话事件名称为 `session:start`、`session:update`、`session:end`，以及环境检测后的 `tool:detect`。会话 start/end 表示工作轮次开始/结束。事件提供工具、会话标识、状态等元数据，不附带工具输出、审批凭据或图片数据。

事件观察不是持久消息队列：单条最多 64 KiB、最多排队 64 条，过大事件或队列繁忙时会丢弃观察通知，优先保持对话正常。插件需将真正需要恢复的自身状态写入 `storage`。

## 故障和资源边界

- 每插件独立 Worker；激活默认 5 秒、钩子 1 秒、界面操作 2 秒、事件 1 秒、停用回调 1 秒。心跳会结束卡住的线程，界面显示运行错误并撤掉入口，可手动重新启用。
- 默认每 Worker 旧生代内存上限 64 MiB；最多安装 32 个插件，每包 16 MiB/512 条目，入口 1 MiB。
- 每插件最多 12 个面板、8 个快捷入口、每面板 12 个按钮、128 个定时器。跨线程消息有大小和排队上限。
- 业务钩子超时、不可达、输出不合法或超限时，继续使用原请求/响应，不导致基础对话失败。
- 桌面与核心通过带随机令牌的本机连接通信，每 30 秒续租，90 秒未续租失效。桌面重新打开可更新已有核心连接；旧进程退出不会清掉新进程的连接。短暂断连自动重试，持续失败才在插件页提示。

## 开发验证

```powershell
node --test --test-concurrency=1 remote/bridge/desktop/test/plugin-loader.test.cjs remote/bridge/desktop/test/plugin-ipc.test.cjs remote/bridge/desktop/test/plugins-ui.test.cjs
```

Go 回归：在 `remote/bridge` 下运行 `go test ./internal/pluginhook ./internal/gateway ./internal/apiservice ./internal/console`。

原生 Electron 验收脚本为 `desktop/scripts/test-plugins-electron.cjs`：由该项目的 Electron 可执行文件运行，将 `SALCARA_PLUGIN_TEST_OUTPUT` 设为绝对输出目录。它使用临时用户目录、真实 preload/IPC/Worker/UI 与模拟核心；验证安装确认、面板操作、通知派发、页面重载、后创建悬浮球、菜单左右与窄幅布局、Esc 释放点击拦截、真实 HTTP 401 后续凭据重连、页面隐藏恢复、启停、宿主重启、窄窗口和卸载，不接触正常配置或真实模型服务。输出 `fixture-root.txt` 记录临时目录，进程退出后可删除该目录。系统通知派发被测试接收器捕获，未验证 Windows 的通知显示设置。

悬浮球的原生显示器/拖动边界和页面恢复回归分别为 `desktop/test/ball-ipc.test.cjs`、`desktop/test/ball-renderer.test.cjs`。原生显示器数据和页面可见性由测试驱动；不等同于在每一种真实显示器与 DPI 组合下手动拖动验收。
