// 检索提取脚本（extension/search.js）的离线回归。
//
//   node internal/feishuweb/testdata/search-regress.mjs            离线：造的页面，比对 search.json
//   node internal/feishuweb/testdata/search-regress.mjs --write    离线：重写 search.json
//   node internal/feishuweb/testdata/search-regress.mjs --live     另加：访客身份打开公开站点 waytoagi.feishu.cn 的搜索页，应当回 login_required
//
// 用隔离的无头 Chrome（全新空 profile，没有任何 cookie / 登录态，跑完删除）。离线用例里
// 所有请求都被拦下：x.feishu.cn 的搜索页由本地 fixture 应答，其余一律拒绝，不出网。
// 需要 macOS 上的 Google Chrome（或用 CHROME 环境变量指定路径）和 Node 22+。
import { spawn } from 'node:child_process';
import { readFileSync, existsSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';

const HERE = dirname(fileURLToPath(import.meta.url));
const SRC = readFileSync(join(HERE, '..', 'extension', 'search.js'), 'utf8');
const WRITE = process.argv.includes('--write');
const LIVE = process.argv.includes('--live');
const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

const PAGES = {
  'https://x.feishu.cn/drive/search': readFileSync(join(HERE, 'search-page.html'), 'utf8'),
  'https://empty.feishu.cn/drive/search': '<!doctype html><meta charset="utf-8"><body><input placeholder="搜索" value="不存在的词"><div class="search-empty">没有找到相关结果</div></body>',
  'https://unknown.feishu.cn/drive/search': '<!doctype html><meta charset="utf-8"><body><input placeholder="搜索" value="周报"><div class="search-result-list"><div class="row" data-token="AbCdEf1234567890">周报</div></div></body>',
  'https://accounts.feishu.cn/accounts/page/login': '<!doctype html><body>Log in</body>',
};

const profile = mkdtempSync(join(tmpdir(), 'fw-search-regress-'));
const chrome = spawn(CHROME, ['--headless=new', `--user-data-dir=${profile}`, '--remote-debugging-port=0',
  '--remote-debugging-address=127.0.0.1', '--no-first-run', '--no-default-browser-check',
  '--disable-extensions', '--disable-sync', 'about:blank'], { stdio: 'ignore' });
let failed = 0;
try {
  let port;
  for (let i = 0; i < 50 && !port; i++) {
    await sleep(200);
    const f = join(profile, 'DevToolsActivePort');
    if (existsSync(f)) port = readFileSync(f, 'utf8').split('\n');
  }
  const ws = new WebSocket(`ws://127.0.0.1:${port[0]}${port[1]}`);
  await new Promise((r) => { ws.onopen = r; });
  let id = 0;
  const pend = new Map();
  const handlers = [];
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } else if (m.method) handlers.forEach((h) => h(m));
  };
  const send = (method, params = {}, sessionId) => new Promise((r) => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method, params, sessionId })); });

  const run = async (url, opts, offline = true) => {
    const { result: { targetId } } = await send('Target.createTarget', { url: 'about:blank' });
    const { result: { sessionId } } = await send('Target.attachToTarget', { targetId, flatten: true });
    const S = (m, p) => send(m, p, sessionId);
    if (offline) {
      handlers.length = 0;
      handlers.push((m) => {
        if (m.method !== 'Fetch.requestPaused' || m.sessionId !== sessionId) return;
        const u = m.params.request.url.split(/[?#]/)[0];
        if (PAGES[u] !== undefined) {
          S('Fetch.fulfillRequest', { requestId: m.params.requestId, responseCode: 200,
            responseHeaders: [{ name: 'Content-Type', value: 'text/html; charset=utf-8' }],
            body: Buffer.from(PAGES[u]).toString('base64') });
        } else {
          S('Fetch.failRequest', { requestId: m.params.requestId, errorReason: 'BlockedByClient' });
        }
      });
      await S('Fetch.enable', { patterns: [{ urlPattern: '*' }] });
    }
    await S('Page.enable');
    await S('Page.navigate', { url });
    await sleep(offline ? 1500 : 10000);
    const r = await S('Runtime.evaluate', { expression: `${SRC}\n;feishuSearchExtract(${JSON.stringify(opts)})`, returnByValue: true, awaitPromise: true });
    await send('Target.closeTarget', { targetId });
    const v = r.result?.result?.value;
    if (!v) throw new Error('evaluate failed: ' + JSON.stringify(r.result?.exceptionDetails || r.error));
    return v;
  };
  const check = (name, cond, detail) => {
    console.log(`${cond ? 'PASS' : 'FAIL'} ${name}${cond ? '' : ' ' + JSON.stringify(detail)}`);
    if (!cond) failed++;
  };

  const main = await run('https://x.feishu.cn/drive/search?query=%E5%91%A8%E6%8A%A5', { query: '周报', waitMs: 8000, interval: 300 });
  const golden = join(HERE, 'search.json');
  if (WRITE) {
    writeFileSync(golden, JSON.stringify({ status: main.status, url: main.url, items: main.items }, null, 2) + '\n');
    console.log('wrote', golden);
  } else {
    const want = JSON.parse(readFileSync(golden, 'utf8'));
    check('fixture 结果与 search.json 一致', JSON.stringify(want.items) === JSON.stringify(main.items) && want.status === main.status, main);
  }
  check('fixture 走 result_box 策略、跳过侧栏', main.searchDiag.strategy === 'result_box' && !main.items.some((i) => i.url.includes('RecentDoc')), main.searchDiag);

  const empty = await run('https://empty.feishu.cn/drive/search?query=x', { query: '不存在的词', waitMs: 9000, interval: 300 });
  check('空结果回 ok + 0 条', empty.status === 'ok' && empty.items.length === 0 && empty.searchDiag.emptyHint, empty);

  const unknown = await run('https://unknown.feishu.cn/drive/search?query=x', { query: '周报', waitMs: 4000, interval: 300 });
  check('认不出结构回 structure_unknown 带 class 样本', unknown.status === 'structure_unknown' && unknown.searchDiag.classes.length > 0, unknown);

  const login = await run('https://accounts.feishu.cn/accounts/page/login?redirect_uri=x', { query: '周报' });
  check('登录页回 login_required', login.status === 'login_required', login);

  if (LIVE) {
    // 公开站点，访客身份：搜索页需要登录，应当被跳到 accounts.feishu.cn。
    const live = await run('https://waytoagi.feishu.cn/drive/search?query=AGI', { query: 'AGI' }, false);
    check('公开站点访客检索回 login_required', live.status === 'login_required', live);
  }
  ws.close();
} finally {
  chrome.kill('SIGTERM');
  await sleep(500);
  rmSync(profile, { recursive: true, force: true });
}
if (failed) { console.log(`${failed} 项失败`); process.exit(1); }
console.log('全部通过');
