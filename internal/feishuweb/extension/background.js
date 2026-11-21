// mcp-hub 飞书文档读取 · service worker
//
// 扩展主动连到本机 mcp-hub 配置页面（ws://127.0.0.1:<端口>/ext/ws），第一条消息
// 带配对 token；之后 mcp-hub 推任务过来，扩展在后台标签页里打开文档、注入
// extract.js 读块树、把结果回传、关掉标签页。
//
// 这里没有任何写路径：不改文档、不发消息、不调飞书接口、不读 cookie。
importScripts('extract.js');

const DEFAULT_PORT = 7801;
const DOC_URL = /^https:\/\/[a-z0-9-]+(\.[a-z0-9-]+)*\.(feishu\.cn|larksuite\.com|larkoffice\.com)\/(docx|wiki)\/[A-Za-z0-9]{8,64}$/;
const DOC_PATTERNS = ['feishu.cn', 'larksuite.com', 'larkoffice.com'].flatMap((d) =>
  ['docx', 'wiki'].map((k) => `https://*.${d}/${k}/*`));

let ws = null;
let pingTimer = null;
let retryTimer = null;
let backoff = 1000;

async function setState(state, detail) {
  await chrome.storage.local.set({ state, stateDetail: detail || '', stateAt: Date.now() });
}

async function connect() {
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;
  const { token, port } = await chrome.storage.local.get(['token', 'port']);
  if (!token) {
    await setState('unpaired', '还没填配对码');
    return;
  }
  const sock = new WebSocket(`ws://127.0.0.1:${Number(port) || DEFAULT_PORT}/ext/ws`);
  ws = sock;
  sock.onopen = () => {
    let installType = '';
    chrome.management.getSelf().then((s) => { installType = s.installType; }).catch(() => {}).finally(() => {
      sock.send(JSON.stringify({ type: 'hello', token, version: chrome.runtime.getManifest().version, installType }));
    });
  };
  sock.onmessage = (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.type === 'welcome') {
      backoff = 1000;
      setState('connected');
      clearInterval(pingTimer);
      // 活跃的 WebSocket 会延长 MV3 service worker 的寿命（Chrome 116+），
      // 前提是 30 秒内有收发。
      pingTimer = setInterval(() => { if (sock.readyState === WebSocket.OPEN) sock.send('{"type":"ping"}'); }, 20000);
    } else if (m.type === 'job' && m.job) {
      runJob(m.job).then((result) => send(sock, { type: 'result', id: m.id, result }),
        (err) => send(sock, { type: 'result', id: m.id, result: { status: 'error', error: String(err && err.message || err) } }));
    }
  };
  sock.onclose = (ev) => {
    clearInterval(pingTimer);
    if (ws === sock) ws = null;
    const paired = ev.code !== 1008;
    setState(paired ? 'disconnected' : 'unpaired', paired ? 'mcp-hub 没在运行或连接断了' : '配对码不对或已失效，请在 mcp-hub 里重新生成');
    if (paired) scheduleRetry();
  };
}

function send(sock, msg) {
  if (sock.readyState === WebSocket.OPEN) sock.send(JSON.stringify(msg));
}

function scheduleRetry() {
  clearTimeout(retryTimer);
  retryTimer = setTimeout(connect, backoff);
  backoff = Math.min(backoff * 2, 60000);
}

async function runJob(job) {
  if (job.kind === 'read_doc') return readDoc(job.url);
  if (job.kind === 'recent') return recent();
  return { status: 'not_implemented', error: 'unknown job ' + job.kind };
}

// 后台标签页打开文档 -> 等加载完 -> MAIN world 执行 feishuExtract -> 关标签页。
async function readDoc(url) {
  if (!DOC_URL.test(url)) return { status: 'error', error: '地址不是飞书 / Lark 的 docx 或 wiki 页面' };
  const tab = await chrome.tabs.create({ url, active: false });
  try {
    await waitComplete(tab.id, 45000);
    const [res] = await chrome.scripting.executeScript({
      target: { tabId: tab.id }, world: 'MAIN', func: feishuExtract, args: [{}],
    });
    const out = res && res.result;
    if (!out) return { status: 'error', error: '提取脚本没有返回结果' };
    if (out.status === 'ok') await remember({ title: out.title, url });
    return out;
  } finally {
    chrome.tabs.remove(tab.id).catch(() => {});
  }
}

function waitComplete(tabId, ms) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { chrome.tabs.onUpdated.removeListener(on); reject(new Error('页面加载超时')); }, ms);
    function on(id, info) {
      if (id === tabId && info.status === 'complete') {
        clearTimeout(timer);
        chrome.tabs.onUpdated.removeListener(on);
        resolve();
      }
    }
    chrome.tabs.onUpdated.addListener(on);
  });
}

// 最近文档：当前开着的飞书文档标签页 + 扩展读过的文档。只有标题和地址。
async function recent() {
  const tabs = await chrome.tabs.query({ url: DOC_PATTERNS });
  const items = tabs.map((t) => ({ title: t.title || '', url: (t.url || '').split(/[?#]/)[0], source: 'open_tab' }));
  const { history = [] } = await chrome.storage.local.get('history');
  return { status: 'ok', items: items.concat(history.map((h) => ({ ...h, source: 'read_history' }))) };
}

async function remember(item) {
  const { history = [] } = await chrome.storage.local.get('history');
  const next = [{ title: String(item.title || '').slice(0, 200), url: item.url }]
    .concat(history.filter((h) => h.url !== item.url)).slice(0, 50);
  await chrome.storage.local.set({ history: next });
}

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.storage.onChanged.addListener((changes) => {
  if (changes.token || changes.port) {
    if (ws) ws.close();
    backoff = 1000;
    connect();
  }
});
chrome.runtime.onMessage.addListener((msg) => { if (msg === 'reconnect') { backoff = 1000; connect(); } });
connect();
