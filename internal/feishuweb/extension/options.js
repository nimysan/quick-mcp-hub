// 配对页：把 mcp-hub 给的配对码存进 chrome.storage.local（只在本机），
// service worker 监听到变化后会重连。
const $ = (id) => document.getElementById(id);
const TEXT = {
  connected: ['ok', '✅ 已连上 mcp-hub'],
  disconnected: ['bad', '没连上 mcp-hub'],
  unpaired: ['bad', '还没配对'],
};

async function render() {
  const { token = '', port = '', state = 'unpaired', stateDetail = '' } =
    await chrome.storage.local.get(['token', 'port', 'state', 'stateDetail']);
  if (document.activeElement !== $('token')) $('token').value = token;
  if (document.activeElement !== $('port')) $('port').value = port;
  const [cls, label] = TEXT[state] || ['bad', state];
  $('st').className = 'st ' + cls;
  $('st').textContent = label + (stateDetail ? '：' + stateDetail : '');
}

$('save').onclick = async () => {
  const token = $('token').value.trim();
  const p = parseInt($('port').value.trim(), 10);
  await chrome.storage.local.set({ token, port: p > 0 && p < 65536 ? p : '' });
  chrome.runtime.sendMessage('reconnect').catch(() => {});
};

chrome.storage.onChanged.addListener(render);
render();
