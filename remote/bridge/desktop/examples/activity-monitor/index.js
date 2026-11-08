'use strict';
exports.activate = async salcara => {
  let count = Number(await salcara.storage.get('count')) || 0;
  let requests = 0, sessions = 0, usage = 0;
  const content = () => `这个面板由本机插件提供。\n\n已保存记录：${count}\n本次启动的 API 请求：${requests}\n会话状态更新：${sessions}\n用量事件：${usage}\n\n关闭窗口或重新启动后，已保存记录仍会保留。`;
  const panel = salcara.ui.registerPanel('activity', {
    label: '工作动态', title: '工作动态', content: content(),
    actions: [
      { id: 'record', label: '记录一次', onClick: async () => { await salcara.storage.set('count', count + 1); count++; panel.update({ content: content() }); salcara.ui.toast('记录已保存', { type: 'ok' }); } },
      { id: 'notify', label: '发送测试通知', onClick: () => salcara.notify.send('工作动态', '插件通知已经接通。') },
    ],
  });
  salcara.ui.addSatellite('activity', { label: '工作动态', panelId: 'activity' });
  salcara.api.intercept('request', (request, next) => { requests++; panel.update({ content: content() }); return next(request); });
  salcara.api.on('usage', () => { usage++; panel.update({ content: content() }); });
  salcara.events.on('session:update', () => { sessions++; panel.update({ content: content() }); });
};
