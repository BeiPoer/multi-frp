const byId = (id) => document.getElementById(id);
const detail = byId('detail');
const domain = byId('domain');
const chart = byId('chart');
const ctx = chart.getContext('2d');
let session = false;
let currentTunnel;
let chartData;
let polling = false;

async function api(path, body) {
  const options = body === undefined ? {} : {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  };
  const response = await fetch(`/api/admin${path}`, options);
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw Object.assign(new Error(data.error || '无法连接到管理服务'), { status: response.status });
  }
  return response.json();
}

function showMessage(text, error = false) {
  const node = byId('message');
  node.textContent = text;
  node.hidden = !text;
  node.classList.toggle('error', error);
}

function displaySession(value) {
  session = value;
  byId('login').hidden = value;
  byId('dashboard').hidden = !value;
  byId('logout').hidden = !value;
  if (!value) {
    byId('invitation').hidden = true;
    byId('invite-code').textContent = '';
    if (detail.open) detail.close();
  }
}

function element(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}

function addCell(row, ...contents) {
  const cell = element('td');
  cell.append(...contents);
  row.append(cell);
}

function button(label, run, className) {
  const node = element('button', label, className);
  node.type = 'button';
  node.onclick = run;
  return node;
}

function mbps(bits) { return `${(bits / 1e6).toLocaleString('zh-CN', { maximumFractionDigits: 3 })} Mbps`; }

function bytes(count) {
  const divisor = count >= 1e9 ? 1e9 : 1e6;
  return `${(count / divisor).toLocaleString('zh-CN', { maximumFractionDigits: 3 })} ${divisor === 1e9 ? 'GB' : 'MB'}`;
}

function directions(value, format) {
  const incoming = element('div', `↓ ${format(value.in)}`);
  incoming.title = '公网 → 客户端';
  const outgoing = element('div', `↑ ${format(value.out)}`);
  outgoing.title = '客户端 → 公网';
  return [incoming, outgoing];
}

function tunnelStatus(tunnel) {
  if (tunnel.state === 'pending') return ['待审批', 'pending'];
  if (tunnel.state === 'rejected') return ['已拒绝', ''];
  if (!tunnel.enabled) return ['已停用', ''];
  return tunnel.online ? ['在线', 'online'] : ['待连接', ''];
}

function emptyRow(body, columns, text) {
  const cell = element('td', text, 'empty');
  cell.colSpan = columns;
  body.insertRow().append(cell);
}

function render(data) {
  if (document.activeElement !== domain && !domain.dataset.editing) domain.value = data.domain;
  byId('timezone').textContent = `统计时区 ${data.timezone}`;
  const tunnels = byId('tunnels');
  const devices = byId('devices');
  tunnels.replaceChildren();
  devices.replaceChildren();
  if (!data.tunnels.length) emptyRow(tunnels, 6, '暂无隧道申请');
  if (!data.devices.length) emptyRow(devices, 4, '暂无已绑定设备');
  const deviceIndex = new Map(data.devices.map((item) => [item.id, item]));
  for (const tunnel of data.tunnels) {
    const row = tunnels.insertRow();
    addCell(row, element('strong', tunnel.name), element('br'), element('small', tunnel.device));
    addCell(row, element('div', tunnel.address || '—'), element('small', `${tunnel.localIP}:${tunnel.localPort}`));
    const [label, status] = tunnelStatus(tunnel);
    addCell(row, element('span', label, `status ${status}`));
    addCell(row, ...directions(tunnel.rate, mbps));
    addCell(row, ...directions(tunnel.today, bytes));
    const actions = element('div', undefined, 'actions');
    const change = (action) => submit(() => api(`/tunnels/${tunnel.id}`, { action }), '隧道状态已更新');
    if (tunnel.state === 'pending') {
      actions.append(button('批准', () => change('approve')), button('拒绝', () => change('reject'), 'danger'));
    }
    if (tunnel.state === 'approved') {
      if (!deviceIndex.get(tunnel.deviceID)?.revoked) {
        actions.append(button(tunnel.enabled ? '停用' : '启用', () => change(tunnel.enabled ? 'disable' : 'enable')));
      }
      actions.append(button('流量详情', () => openDetail(tunnel)));
    }
    addCell(row, actions);
  }
  for (const item of data.devices) {
    const row = devices.insertRow();
    addCell(row, item.name);
    addCell(row, element('code', item.id));
    addCell(row, element('span', item.revoked ? '已撤销' : '已授权', item.revoked ? 'status' : 'status online'));
    const revoke = () => {
      if (window.confirm(`撤销「${item.name}」的设备授权并断开其所有隧道？`)) {
        submit(() => api(`/devices/${item.id}/revoke`, {}), '设备授权已撤销');
      }
    };
    addCell(row, item.revoked ? '—' : button('撤销授权', revoke, 'danger'));
  }
}

async function refresh() {
  if (polling) return;
  polling = true;
  try {
    const data = await api('/state');
    displaySession(true);
    if (!byId('dashboard').contains(document.activeElement) || document.activeElement === domain ||
        !document.activeElement.closest('table')) render(data);
  } catch (error) {
    if (error.status === 401) displaySession(false);
    else showMessage(error.message, true);
  } finally { polling = false; }
}

async function submit(action, message) {
  try {
    await action();
    showMessage(message);
    if (document.activeElement.closest('table')) document.activeElement.blur();
    await refresh();
  } catch (error) {
    if (error.status === 401 && session) displaySession(false);
    showMessage(error.message, true);
  }
}

byId('login-form').onsubmit = (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const credentials = Object.fromEntries(new FormData(form));
  submit(async () => { await api('/login', credentials); form.reset(); }, '');
};
byId('logout').onclick = async () => {
  try { await api('/logout', {}); displaySession(false); showMessage(''); }
  catch (error) { showMessage(error.message, true); }
};
domain.oninput = () => { domain.dataset.editing = '1'; };
byId('domain-form').onsubmit = (event) => {
  event.preventDefault();
  submit(async () => {
    await api('/domain', { domain: domain.value });
    delete domain.dataset.editing;
    domain.blur();
  }, '域名已保存');
};
byId('invite').onclick = () => submit(async () => {
  const invite = await api('/invites', {});
  byId('invite-code').textContent = invite.code;
  byId('invite-expiry').textContent = `有效期至 ${new Date(invite.expires).toLocaleString('zh-CN')}`;
  byId('invitation').hidden = false;
}, '');
byId('copy-invite').onclick = async () => {
  try { await navigator.clipboard.writeText(byId('invite-code').textContent); showMessage('邀请码已复制'); }
  catch { showMessage('可选中邀请码手动复制', true); }
};

function openDetail(tunnel) {
  currentTunnel = tunnel.id;
  byId('detail-title').textContent = `${tunnel.name} · 流量详情`;
  byId('period').value = '24h';
  detail.showModal();
  loadChart();
}

async function loadChart() {
  const id = currentTunnel;
  const range = byId('period').value;
  byId('chart-point').textContent = '';
  byId('detail-total').textContent = '读取中…';
  ctx.clearRect(0, 0, chart.width, chart.height);
  chartData = undefined;
  try {
    const data = await api(`/tunnels/${id}/history?range=${range}`);
    if (!detail.open || id !== currentTunnel || range !== byId('period').value) return;
    chartData = data;
    byId('detail-total').textContent = `公网 → 客户端 ${bytes(data.total.in)} · 客户端 → 公网 ${bytes(data.total.out)}`;
    drawChart(data);
  } catch (error) { byId('detail-total').textContent = error.message; }
}

function drawChart(data) {
  const { points, seconds, timezone } = data;
  const left = 85, right = chart.width - 16, top = 15, base = chart.height - 35;
  const maximum = Math.max(125000, ...points.map((point) => Math.max(point.in, point.out) * 8 / seconds));
  const xAt = (index) => left + (right - left) * index / (points.length - 1);
  ctx.font = '12px sans-serif';
  ctx.fillStyle = '#526177';
  ctx.strokeStyle = '#dce2ec';
  for (let i = 0; i <= 4; i++) {
    const y = base - (base - top) * i / 4;
    ctx.fillText(mbps(maximum * i / 4), 0, y + 4);
    ctx.beginPath(); ctx.moveTo(left, y); ctx.lineTo(right, y); ctx.stroke();
  }
  for (let i = 0; i <= 4; i++) {
    const at = Math.round((points.length - 1) * i / 4);
    const label = new Date(points[at].time * 1000).toLocaleString('zh-CN', { timeZone: timezone, month: 'numeric', day: 'numeric', hour: '2-digit' });
    ctx.fillText(label, Math.min(xAt(at) - 30, right - 58), base + 24);
  }
  for (const [key, color] of [['in', '#205cd4'], ['out', '#0d896f']]) {
    ctx.strokeStyle = color;
    ctx.lineWidth = 2;
    ctx.beginPath();
    points.forEach((point, index) => {
      const y = base - (base - top) * point[key] * 8 / seconds / maximum;
      if (index === 0) ctx.moveTo(xAt(index), y);
      else ctx.lineTo(xAt(index), y);
    });
    ctx.stroke();
  }
  ctx.lineWidth = 1;
}

chart.onpointermove = (event) => {
  if (!chartData) return;
  const { points, seconds, timezone } = chartData;
  const x = event.offsetX * chart.width / chart.clientWidth;
  const index = Math.max(0, Math.min(points.length - 1, Math.round((x - 85) / (chart.width - 101) * (points.length - 1))));
  const point = points[index];
  const time = new Date(point.time * 1000).toLocaleString('zh-CN', { timeZone: timezone });
  byId('chart-point').textContent = `${time} · ↓ ${mbps(point.in * 8 / seconds)} (${bytes(point.in)}) · ↑ ${mbps(point.out * 8 / seconds)} (${bytes(point.out)})`;
};

byId('period').onchange = loadChart;
refresh();
setInterval(() => { if (session) refresh(); }, 5000);
