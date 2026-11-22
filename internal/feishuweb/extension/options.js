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

// 检索选择器覆盖：只接受 {trigger,input,panel,item} 里的字符串数组，别的键丢掉。
const SEL_KEYS = ['trigger', 'input', 'panel', 'item'];
$('saveSel').onclick = async () => {
  const raw = $('selectors').value.trim();
  if (!raw) {
    await chrome.storage.local.set({ searchSelectors: '' });
    $('selst').textContent = '已清空，使用内置候选';
    return;
  }
  let v;
  try { v = JSON.parse(raw); } catch (e) { $('selst').textContent = 'JSON 格式不对：' + e.message; return; }
  const out = {};
  for (const k of SEL_KEYS) {
    if (Array.isArray(v && v[k]) && v[k].every((x) => typeof x === 'string' && x.length < 200)) out[k] = v[k].slice(0, 30);
  }
  if (!Object.keys(out).length) { $('selst').textContent = '没有可用的选择器组（键只能是 ' + SEL_KEYS.join(' / ') + '，值是字符串数组）'; return; }
  await chrome.storage.local.set({ searchSelectors: JSON.stringify(out) });
  $('selst').textContent = '已保存：' + Object.keys(out).join(' / ');
};
chrome.storage.local.get('searchSelectors').then(({ searchSelectors = '' }) => { $('selectors').value = searchSelectors; });

chrome.storage.onChanged.addListener(render);
render();
