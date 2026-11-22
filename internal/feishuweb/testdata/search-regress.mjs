// 扩展页面脚本的离线回归：检索（extension/search.js）和读文档的页面去向判断（extension/extract.js）。
//
//   node internal/feishuweb/testdata/search-regress.mjs            离线：造的页面，比对 search.json
//   node internal/feishuweb/testdata/search-regress.mjs --write    离线：重写 search.json
//   node internal/feishuweb/testdata/search-regress.mjs --live     另加：访客身份打开公开站点 waytoagi.feishu.cn 的首页，
//                                                                  看搜索入口在访客页面上是否存在（只读，不登录）
//
// 用隔离的无头 Chrome（全新空 profile，没有任何 cookie / 登录态，跑完删除）。离线用例里
// 所有请求都被拦下：造的飞书页面由本地 fixture 应答，其余一律拒绝，不出网。
// 需要 macOS 上的 Google Chrome（或用 CHROME 环境变量指定路径）和 Node 22+。
import { spawn } from 'node:child_process';
import { readFileSync, existsSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';

const HERE = dirname(fileURLToPath(import.meta.url));
const SEARCH_SRC = readFileSync(join(HERE, '..', 'extension', 'search.js'), 'utf8');
const EXTRACT_SRC = readFileSync(join(HERE, '..', 'extension', 'extract.js'), 'utf8');
const WRITE = process.argv.includes('--write');
const LIVE = process.argv.includes('--live');
const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

const HOME = readFileSync(join(HERE, 'search-page.html'), 'utf8');
const page = (body) => `<!doctype html><meta charset="utf-8"><body>${body}</body>`;
const PAGES = {
  'https://accounts.feishu.cn/accounts/page/login': page('Log in'),
  'https://www.feishu.cn/404': page('404 This page isn\'t available'),
  // 读文档：假 token 的几种页面去向（BUG-02）。
  'https://r.feishu.cn/docx/NotFound00000001': page('<div class="err">文档不存在或已被删除</div>'),
  'https://r.feishu.cn/wiki/NoPerm0000000001': page('<div>你没有访问权限 <button>申请权限</button></div>'),
  'https://r.feishu.cn/docx/Blank00000000001': page('<div id="app"></div>'),
  // 长页面：错误提示在空状态容器里，侧栏目录里有「404」字样的标题不该误判。
  'https://r.feishu.cn/wiki/LongNF0000000001': page('<nav>' + '目录项 '.repeat(80) + '<a>404 复盘</a></nav><div class="ud-empty-state">This document has been deleted</div>'),
  'https://r.feishu.cn/wiki/LongOK0000000001': page('<nav>' + '目录项 '.repeat(80) + '<a>404 复盘</a><a>没有权限怎么办</a></nav><div id="app"></div>'),
  'https://r.feishu.cn/docx/Shell00000000001': page('<div id="mainBox">加载中</div>'),
  'https://r.feishu.cn/docx/Redir00000000001': page('<script>setTimeout(() => location.replace("https://www.feishu.cn/404"), 300)</script>'),
};
const fixture = (u) => {
  if (PAGES[u] !== undefined) return PAGES[u];
  if (/^https:\/\/[a-z]+\.feishu\.cn\/drive\/home\/$/.test(u)) return HOME;
  return undefined;
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

  const run = async (url, src, call, offline = true) => {
    const { result: { targetId } } = await send('Target.createTarget', { url: 'about:blank' });
    const { result: { sessionId } } = await send('Target.attachToTarget', { targetId, flatten: true });
    const S = (m, p) => send(m, p, sessionId);
    if (offline) {
      handlers.length = 0;
      handlers.push((m) => {
        if (m.method !== 'Fetch.requestPaused' || m.sessionId !== sessionId) return;
        const body = fixture(m.params.request.url.split(/[?#]/)[0]);
        if (body !== undefined) {
          S('Fetch.fulfillRequest', { requestId: m.params.requestId, responseCode: 200,
            responseHeaders: [{ name: 'Content-Type', value: 'text/html; charset=utf-8' }],
            body: Buffer.from(body).toString('base64') });
        } else {
          S('Fetch.failRequest', { requestId: m.params.requestId, errorReason: 'BlockedByClient' });
        }
      });
      await S('Fetch.enable', { patterns: [{ urlPattern: '*' }] });
    }
    await S('Page.enable');
    await S('Page.navigate', { url });
    await sleep(offline ? 800 : 10000);
    const t = Date.now();
    const r = await S('Runtime.evaluate', { expression: `${src}\n;${call}`, returnByValue: true, awaitPromise: true });
    const ms = Date.now() - t;
    await send('Target.closeTarget', { targetId });
    const v = r.result?.result?.value;
    if (!v) throw new Error('evaluate failed: ' + JSON.stringify(r.result?.exceptionDetails || r.error));
    v.ms = ms;
    return v;
  };
  const search = (host, opts, offline) => run(`https://${host}/drive/home/`, SEARCH_SRC,
    `feishuSearchExtract(${JSON.stringify(Object.assign({ query: '周报', interval: 200, waitMs: 9000, shellMs: 3000, openMs: 1000, enterAfterMs: 1500, hotkeyAfterMs: 1000 }, opts))})`, offline);
  const read = (url, opts) => run(url, EXTRACT_SRC, `feishuExtract(${JSON.stringify(Object.assign({ waitMs: 6000, shellMs: 3000 }, opts))})`);
  const check = (name, cond, detail) => {
    console.log(`${cond ? 'PASS' : 'FAIL'} ${name}${cond ? '' : ' ' + JSON.stringify(detail)}`);
    if (!cond) failed++;
  };

  // ── 检索 ───────────────────────────────────────────────
  const main = await search('x.feishu.cn', {});
  const golden = join(HERE, 'search.json');
  if (WRITE) {
    writeFileSync(golden, JSON.stringify({ status: main.status, url: main.url, items: main.items }, null, 2) + '\n');
    console.log('wrote', golden);
  } else {
    const want = JSON.parse(readFileSync(golden, 'utf8'));
    check('点「Search ⌘+J」唤起弹层，结果与 search.json 一致', JSON.stringify(want.items) === JSON.stringify(main.items) && want.status === main.status, main);
  }
  const d = main.searchDiag;
  check('入口靠文字提示找到，结果只取弹层内（不含侧栏、首页、最近访问基线）',
    d.trigger === 'text:hotkey-hint' && d.panel === '[role="dialog"]' && d.baseline === 2 &&
    !main.items.some((i) => /RecentDoc|HomeDoc|Recent000/.test(i.url)), d);

  const hk = await search('hotkey.feishu.cn', {});
  check('没有入口时按 ⌘/Ctrl+J 唤起；结果从 data-* 属性读', hk.status === 'ok' && hk.searchDiag.trigger === 'hotkey' &&
    hk.searchDiag.strategy === 'attributes' && hk.items.length === 2 && hk.items[0].title === '周报 第一篇' &&
    hk.items[1].url === 'https://hotkey.feishu.cn/wiki/AttrWiki12345678', hk);

  const en = await search('enter.feishu.cn', {});
  check('结果不自动出来时按一次回车', en.status === 'ok' && en.searchDiag.enter && en.items.length === 1, en);

  const empty = await search('empty.feishu.cn', {});
  check('空结果回 ok + 0 条', empty.status === 'ok' && empty.items.length === 0 && empty.searchDiag.emptyHint, empty);

  const unknown = await search('unknown.feishu.cn', { waitMs: 5000 });
  check('有结果行没链接回 structure_unknown，带 class 样本', unknown.status === 'structure_unknown' && unknown.searchDiag.rows > 0 && unknown.searchDiag.classes.length > 0, unknown);

  const same = await search('same.feishu.cn', { waitMs: 5000 });
  check('结果一直等于填词前的最近访问，回 structure_unknown 而不是把最近访问当结果', same.status === 'structure_unknown' && !same.items, same);

  const custom = await search('x.feishu.cn', { selectors: { trigger: ['#nope', '#entry'], input: ['input.q'] } });
  check('配置页覆盖的选择器生效', custom.searchDiag.selectors === 'custom' && custom.searchDiag.trigger === '#entry' && custom.status === 'ok' && custom.items.length === 5, custom.searchDiag);

  const login = await run('https://accounts.feishu.cn/accounts/page/login?redirect_uri=x', SEARCH_SRC, 'feishuSearchExtract({query:"周报"})');
  check('登录页回 login_required', login.status === 'login_required', login);

  const nf = await run('https://www.feishu.cn/404', SEARCH_SRC, 'feishuSearchExtract({query:"周报"})');
  check('起始页跳到 www.feishu.cn/404 回 structure_unknown', nf.status === 'structure_unknown' && /404/.test(nf.error), nf);

  // ── 读文档：假 token 不再说成 unsupported_type，并且尽早结束 ─────────
  const r1 = await read('https://r.feishu.cn/docx/NotFound00000001');
  check(`docx 不存在页回 not_found（${r1.ms}ms）`, r1.status === 'not_found' && r1.ms < 3000, r1);
  const r2 = await read('https://r.feishu.cn/wiki/NoPerm0000000001');
  check(`wiki 无权限页回 no_permission（${r2.ms}ms）`, r2.status === 'no_permission' && r2.ms < 3000, r2);
  const r3 = await read('https://r.feishu.cn/docx/Blank00000000001');
  check(`docx 空白页回 not_found 而不是 unsupported_type（${r3.ms}ms）`, r3.status === 'not_found' && r3.ms < 4500, r3);
  const r6 = await read('https://r.feishu.cn/wiki/LongNF0000000001');
  check(`长页面里空状态容器的「已删除」回 not_found（${r6.ms}ms）`, r6.status === 'not_found' && r6.ms < 3000, r6);
  const r7 = await read('https://r.feishu.cn/wiki/LongOK0000000001', { waitMs: 2500, shellMs: 2000 });
  check('侧栏目录里的「404」「没有权限」字样不触发误判（等到外壳超时才结束）', r7.status === 'not_found' && r7.ms >= 1900 && /页面上没有文档内容/.test(r7.error), r7);
  const r4 = await read('https://r.feishu.cn/docx/Shell00000000001', { waitMs: 2000 });
  check('有编辑器外壳等不到引擎回 engine_missing', r4.status === 'engine_missing', r4);
  const r5 = await read('https://r.feishu.cn/docx/Redir00000000001');
  check(`docx 跳到 www.feishu.cn/404 回 not_found（${r5.ms}ms）`, r5.status === 'not_found' && r5.ms < 3000, r5);

  if (LIVE) {
    // 公开站点，访客身份：只看首页上有没有搜索入口（访客应当没有），不输入、不登录。
    const live = await search('waytoagi.feishu.cn', { waitMs: 8000, shellMs: 6000 }, false);
    console.log('INFO 公开站点访客首页：', JSON.stringify({ status: live.status, diag: live.searchDiag }));
  }
  ws.close();
} finally {
  chrome.kill('SIGTERM');
  await sleep(500);
  rmSync(profile, { recursive: true, force: true });
}
if (failed) { console.log(`${failed} 项失败`); process.exit(1); }
console.log('全部通过');
