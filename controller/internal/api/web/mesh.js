// 复用 index.html 中已登录的 API 会话与模板缓存。
'use strict';

function meshMessage(id, message, failed = false) {
  $(id).textContent = message;
  $(id).className = 'msg ' + (failed ? 'err' : 'ok');
}

async function loadMesh() {
  try {
    await loadTemplates();
    const select = $('mesh-template');
    const keep = select.value;
    select.replaceChildren();
    for (const t of templates) {
      const option = document.createElement('option');
      option.value = t.id;
      option.textContent = t.name;
      select.appendChild(option);
    }
    if (templates.some(t => t.id === keep)) select.value = keep;
    fillMeshForm();
    await refreshMeshStatus();
  } catch (e) { meshMessage('mesh-msg', e.message, true); }
}

function fillMeshForm() {
  const t = templates.find(t => t.id === $('mesh-template').value);
  const m = t && t.mesh || {};
  $('mesh-mode').value = m.mode || 'wired';
  $('mesh-band').value = m.band || '5g';
  $('mesh-id').value = m.id || '';
  $('mesh-key').value = m.key || '';
  $('mesh-network').value = m.network || 'lan';
  $('mesh-dedicated').checked = !!m.dedicated;
  meshMessage('mesh-msg', '');
}

$('mesh-template').addEventListener('change', fillMeshForm);
$('mesh-save').addEventListener('click', async () => {
  const button = $('mesh-save');
  button.disabled = true;
  try {
    const id = $('mesh-template').value;
    if (!id) throw new Error('请先选择模板');
    const mode = $('mesh-mode').value;
    const mesh = mode === 'wired' ? {mode} : {
      mode, id: $('mesh-id').value, key: $('mesh-key').value,
      band: $('mesh-band').value, network: $('mesh-network').value.trim(),
      dedicated: $('mesh-dedicated').checked,
    };
    await api('PUT', '/api/v1/templates/' + encodeURIComponent(id) + '/mesh', {mesh});
    await loadTemplates();
    meshMessage('mesh-msg', '已保存。请在设备概览逐台预检并重新下发；同步成功后再断开节点网线。');
  } catch (e) { meshMessage('mesh-msg', e.message, true); }
  finally { button.disabled = false; }
});

async function refreshMeshStatus() {
  try {
    const data = await api('GET', '/api/v1/mesh');
    if (!loggedIn || currentView !== 'mesh') return;
    const host = $('mesh-topology');
    host.replaceChildren();
    const reports = new Map((data.nodes || []).map(n => [n.ap_mac, n]));
    const names = new Map();
    for (const node of data.nodes || []) {
      const dev = deviceCache.find(d => d.mac === node.ap_mac);
      for (const iface of node.interfaces || []) {
        if (iface.mac) names.set(iface.mac.toLowerCase(), dev && dev.hostname || node.ap_mac);
      }
    }
    for (const dev of deviceCache) {
      const node = reports.get(dev.mac);
      const card = document.createElement('div');
      card.className = 'panel';
      const title = document.createElement('h3');
      title.textContent = dev.hostname || dev.mac;
      card.appendChild(title);
      const details = document.createElement('div');
      details.textContent = [dev.mac, dev.ip].filter(Boolean).join(' · ');
      card.appendChild(details);
      const stale = !node || Date.now() / 1000 - node.updated_at > 90;
      const status = document.createElement('p');
      status.textContent = stale ? '状态未知 / 上报已过期' :
        (node.interfaces.length ? '无线回程已配置' : '无无线回程接口（有线或尚未下发）');
      card.appendChild(status);
      for (const iface of node && node.interfaces || []) {
        const line = document.createElement('p');
        const linked = (iface.peers || []).filter(p => p.established);
        line.textContent = iface.radio + ' · ' + (iface.ifname || '无接口') + ' · ' +
          (stale ? '历史记录' : (!iface.up ? '接口未启动' : linked.length ? linked.length + ' 条已建立链路' : '无已建立链路'));
        card.appendChild(line);
        if (iface.error) {
          const error = document.createElement('p');
          error.textContent = iface.error;
          card.appendChild(error);
        }
        for (const peer of iface.peers || []) {
          const link = document.createElement('p');
          link.textContent = '↔ ' + (names.get(peer.mac.toLowerCase()) || peer.mac) + ' · ' +
            (stale ? '历史' : peer.established ? '已建立' : '协商中') +
            (peer.signal_dbm ? ' · ' + peer.signal_dbm + ' dBm' : '');
          card.appendChild(link);
        }
      }
      host.appendChild(card);
    }
    if (!deviceCache.length) host.textContent = '暂无节点；请先安装并连接 ac-agent。';
    meshMessage('mesh-live-msg', '');
  } catch (e) { meshMessage('mesh-live-msg', e.message, true); }
}

$('mesh-refresh').addEventListener('click', refreshMeshStatus);
