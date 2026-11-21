// 搜索页结果提取脚本。background.js 在后台标签页打开
// https://<租户>/drive/search?query=<关键词>（飞书云空间自己的搜索页），然后用
// chrome.scripting.executeScript({world:'MAIN', func: feishuSearchExtract}) 注入执行。
//
// 这个函数会被序列化后注入，所以必须自包含：不能引用外面的任何变量。
//
// 只读页面已经渲染出来的 DOM：不发任何网络请求、不读 cookie、不调飞书的任何内部接口，
// 也不打开结果里的文档。唯一的「操作」是：页面没有按地址里的 query 自动检索时，
// 把关键词填进页面自己的搜索框并按回车 —— 和用户手动检索一样。
//
// 搜索页只有登录后才有，结果列表的 DOM 结构没能事先实测（访客会被跳到登录页），
// 所以这里不依赖具体 class 名：收集页面里所有指向飞书文档的链接，优先取看起来像
// 结果区的容器里的。认不出来时回 structure_unknown，带上诊断数据（只有计数和 class 名）。
/* exported feishuSearchExtract */
async function feishuSearchExtract(opts) {
  const o = Object.assign({ query: '', waitMs: 20000, interval: 500, stableRounds: 3, maxItems: 100, skipPageCheck: false }, opts || {});
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const t0 = Date.now();
  const diag = {
    at: new Date().toISOString(), host: location.host, path: location.pathname.slice(0, 120),
    login: false, inputFound: false, typed: false, strategy: '', anchors: 0, candidates: 0, emptyHint: false, waitedMs: 0,
  };
  const DOC_PATH = /^\/(docx|docs|wiki|sheets|base|mindnotes|slides|file)\/[A-Za-z0-9]{8,64}(\/|$)/;
  const FEISHU = /(\.feishu\.cn|\.larksuite\.com|\.larkoffice\.com)$/;
  const EMPTY = /没有找到|无搜索结果|暂无(搜索)?结果|未找到(相关)?|No results|No matching|Nothing found/i;
  const RESULT_BOX = '[class*="search" i][class*="result" i],[class*="search" i][class*="list" i],[class*="result" i][class*="list" i],[role="listbox"],[role="list"],[role="grid"],[role="table"]';
  const SKIP_BOX = 'nav,header,aside,[role="navigation"],[class*="sidebar" i],[class*="side-bar" i],[class*="navbar" i],[class*="header" i]';

  // skipPageCheck 只给离线回归用（在本地 fixture 页面上跑）；链接的域名检查不受它影响。
  if (!o.skipPageCheck) {
    if (/^(accounts|passport)\./.test(location.hostname) || /\/login/.test(location.pathname)) {
      diag.login = true;
      return { status: 'login_required', url: location.origin + location.pathname, searchDiag: diag };
    }
    if (!FEISHU.test(location.hostname) || !/^\/drive\/search/.test(location.pathname)) {
      return { status: 'error', error: '搜索页跳到了 ' + location.hostname + location.pathname.slice(0, 40), url: '', searchDiag: diag };
    }
  }

  const clean = (s) => String(s || '').replace(/[\u200b\u200e\u200f\u2060\ufeff]/g, '').replace(/\s+/g, ' ').trim();
  const visible = (e) => !!(e.offsetWidth || e.offsetHeight || e.getClientRects().length);
  const findInput = () => [...document.querySelectorAll('input[type="search"],input[type="text"],input:not([type])')]
    .find((e) => visible(e) && /搜索|search|查找/i.test((e.getAttribute('placeholder') || '') + ' ' + (e.getAttribute('aria-label') || '') + ' ' + String(e.className || '')));

  // 结果条目的标题：链接自己的文字，退到 title / aria-label，再退到所在行的第一行文字。
  const titleOf = (a) => {
    let t = clean(a.innerText || a.textContent);
    if (!t) t = clean(a.getAttribute('title') || a.getAttribute('aria-label'));
    if (!t) {
      const row = a.closest('[role="row"],[role="option"],[role="listitem"],li,tr,[class*="item" i]');
      if (row) t = clean(String(row.innerText || '').split('\n')[0]);
    }
    return t.slice(0, 300);
  };

  const collect = () => {
    const all = [...document.querySelectorAll('a[href]')];
    diag.anchors = all.length;
    const docLinks = all.filter((a) => {
      let u;
      try { u = new URL(a.getAttribute('href'), location.href); } catch { return false; }
      return u.protocol === 'https:' && FEISHU.test(u.hostname) && DOC_PATH.test(u.pathname);
    });
    const boxes = [...document.querySelectorAll(RESULT_BOX)].filter((b) => !b.closest(SKIP_BOX));
    let picked = docLinks.filter((a) => boxes.some((b) => b.contains(a)));
    diag.strategy = 'result_box';
    if (!picked.length) {
      picked = docLinks.filter((a) => !a.closest(SKIP_BOX));
      diag.strategy = 'doc_links';
    }
    // 整个应用都包在某个带 header / sidebar 字样的容器里时，上面两步会全滤掉。
    if (!picked.length && docLinks.length) {
      picked = docLinks;
      diag.strategy = 'all_doc_links';
    }
    return picked.slice(0, o.maxItems).map((a) => {
      const u = new URL(a.getAttribute('href'), location.href);
      return { title: titleOf(a), url: u.origin + u.pathname };
    });
  };

  let items = [];
  let lastKey = '';
  let stable = 0;
  while (Date.now() - t0 < o.waitMs) {
    const input = findInput();
    if (input) diag.inputFound = true;
    // 页面没按地址里的 query 自动检索：填进页面自己的搜索框，和用户手动检索一样。
    if (input && o.query && !diag.typed && clean(input.value) !== clean(o.query) && Date.now() - t0 > 3000) {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      input.focus();
      setter.call(input, o.query);
      input.dispatchEvent(new Event('input', { bubbles: true }));
      for (const type of ['keydown', 'keypress', 'keyup']) {
        input.dispatchEvent(new KeyboardEvent(type, { key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true }));
      }
      diag.typed = true;
    }
    items = collect();
    const key = items.map((i) => i.url).join('|');
    if (items.length && key === lastKey) stable++; else stable = 0;
    lastKey = key;
    if (items.length && stable >= o.stableRounds) break;
    if (!items.length && EMPTY.test(document.body ? document.body.innerText : '') && Date.now() - t0 > 6000) {
      diag.emptyHint = true;
      break;
    }
    await sleep(o.interval);
  }
  diag.waitedMs = Date.now() - t0;
  diag.candidates = items.length;
  const page = location.origin + location.pathname;
  if (items.length) return { status: 'ok', url: page, items, searchDiag: diag };
  if (diag.emptyHint) return { status: 'ok', url: page, items: [], searchDiag: diag };
  // 认不出来：留 class 名样本（不含内容）方便排查。
  const classes = new Set();
  for (const e of document.querySelectorAll('[class*="search" i],[class*="result" i],[class*="list" i]')) {
    const c = String(e.className && e.className.baseVal !== undefined ? e.className.baseVal : e.className || '').trim();
    if (c) classes.add(c.slice(0, 120));
    if (classes.size >= 20) break;
  }
  diag.classes = [...classes];
  return { status: 'structure_unknown', url: page, searchDiag: diag };
}
