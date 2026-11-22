// 检索弹层驱动脚本。background.js 在后台标签页打开 https://<租户>/drive/home/
// （任一已登录页面都行），然后用
// chrome.scripting.executeScript({world:'MAIN', func: feishuSearchExtract}) 注入执行。
//
// 为什么不是打开一个搜索页地址：2026-11-22 用户在真实租户实测，/drive/search?query=
// 会跳到 www.feishu.cn/404，/next/search 是 "This page isn't available"。飞书网页版的
// 检索是首页左上角「Search ⌘+J」唤起的单页弹层，没有能直接打开的搜索页路由。
//
// 这个函数会被序列化后注入，所以必须自包含：不能引用外面的任何变量。
//
// 只读页面已经渲染出来的 DOM：不发任何网络请求、不读 cookie、不调飞书的任何内部接口，
// 也不打开结果里的文档。唯一的「操作」是用户自己也会做的那几步：点搜索框（或按
// ⌘/Ctrl+J）唤起弹层，把关键词填进弹层的输入框；结果没有自动出来时再按一次回车。
//
// 弹层的 DOM 结构只有登录后才看得到，没能事先实测（访客页面上没有搜索入口），所以
// 唤起入口、输入框、弹层容器、结果行全是「多候选」选择器，调用方可以用
// opts.selectors 整组替换（扩展配置页里的「高级：检索选择器」）。结果从弹层里收集
// 指向飞书文档的链接：<a href> 或任何属性值里带文档地址的元素。认不出来时回
// structure_unknown，带上诊断数据（只有计数、命中的选择器和 class 名，不含内容）。
/* exported feishuSearchExtract */
async function feishuSearchExtract(opts) {
  const DEFAULTS = {
    // 唤起入口。除了这些，还会找文字是「搜索 / Search」并带 ⌘J / Ctrl+J 提示的元素。
    trigger: [
      '[data-e2e*="search" i]', '[data-testid*="search" i]', '[data-test-id*="search" i]',
      '[aria-label*="搜索"]', '[aria-label*="search" i]',
      '[class*="search-entry" i]', '[class*="searchEntry" i]', '[class*="search-trigger" i]',
      '[class*="search-box" i]', '[class*="searchBox" i]', '[class*="search-bar" i]', '[class*="searchBar" i]',
      '[class*="search-input" i]', '[class*="searchInput" i]', '[class*="search" i][role="button"]',
    ],
    // 弹层里的输入框（在弹层容器里优先）。
    input: [
      '[role="dialog"] input', '[role="combobox"] input', 'input[role="combobox"]', 'input[type="search"]',
      'input[placeholder*="搜索"]', 'input[placeholder*="search" i]', 'input[aria-label*="搜索"]', 'input[aria-label*="search" i]',
      '[class*="search" i] input', '[class*="modal" i] input',
      '[contenteditable="true"][class*="search" i]', '[class*="search" i] [contenteditable="true"]',
    ],
    // 弹层容器：从输入框往上找第一个命中的；都不命中就取输入框所在的、挂在 body 下的那一层。
    panel: [
      '[role="dialog"]', '[class*="search-modal" i]', '[class*="searchModal" i]', '[class*="search-panel" i]',
      '[class*="searchPanel" i]', '[class*="search-popover" i]', '[class*="search-container" i]',
      '[class*="modal" i]', '[class*="popover" i]', '[class*="overlay" i]',
    ],
    // 结果行：用来取标题、判断「有行但没链接」（结构认不出来）。
    item: [
      '[role="option"]', '[role="listitem"]', '[role="row"]', '[class*="result-item" i]', '[class*="resultItem" i]',
      '[class*="search-item" i]', '[class*="searchItem" i]', '[class*="list-item" i]', '[class*="listItem" i]', 'li',
    ],
  };
  const o = Object.assign({ query: '', waitMs: 25000, shellMs: 12000, openMs: 2500, enterAfterMs: 4000, hotkeyAfterMs: 4000, interval: 300, stableRounds: 3, maxItems: 100 }, opts || {});
  const sel = {};
  let custom = false;
  for (const k of Object.keys(DEFAULTS)) {
    const v = o.selectors && o.selectors[k];
    if (Array.isArray(v) && v.length && v.every((x) => typeof x === 'string')) { sel[k] = v; custom = true; } else sel[k] = DEFAULTS[k];
  }
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const t0 = Date.now();
  const diag = {
    at: new Date().toISOString(), host: location.host, path: location.pathname.slice(0, 120),
    login: false, selectors: custom ? 'custom' : 'default', trigger: '', inputFound: false, panel: '', typed: false,
    enter: false, baseline: 0, rows: 0, strategy: '', anchors: 0, candidates: 0, emptyHint: false, waitedMs: 0,
  };
  const DOC_PATH = /^\/(docx|docs|wiki|sheets|base|mindnotes|slides|file)\/[A-Za-z0-9]{8,64}(\/|$)/;
  const DOC_IN_ATTR = /(?:https:\/\/[a-z0-9.-]+)?\/(?:docx|docs|wiki|sheets|base|mindnotes|slides|file)\/[A-Za-z0-9]{8,64}/;
  const FEISHU = /(\.feishu\.cn|\.larksuite\.com|\.larkoffice\.com)$/;
  const EMPTY = /没有找到|无搜索结果|暂无(搜索)?结果|未找到(相关)?|No results|No matching|Nothing found|no result/i;
  const HOTKEY_HINT = /(⌘|Ctrl|Cmd)\s*\+?\s*J/i;
  const page = () => location.origin + location.pathname;
  const done = (status, extra) => {
    diag.waitedMs = Date.now() - t0;
    return Object.assign({ status, url: page(), searchDiag: diag }, extra || {});
  };

  if (/^(accounts|passport)\./.test(location.hostname) || /\/(login|passport)/.test(location.pathname)) {
    diag.login = true;
    return done('login_required');
  }
  if (!FEISHU.test(location.hostname) || /^www\./.test(location.hostname) || /^\/404/.test(location.pathname)) {
    return done('structure_unknown', { error: '起始页跳到了 ' + location.hostname + location.pathname.slice(0, 40) });
  }

  const clean = (s) => String(s || '').replace(/[\u200b-\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]/g, '').replace(/\s+/g, ' ').trim();
  const visible = (e) => !!(e && e.isConnected && (e.offsetWidth || e.offsetHeight || e.getClientRects().length));
  // 同一组里命中多个互相嵌套的元素时取最里层：合成的点击事件只往上冒泡，
  // 点外层容器碰不到里层元素上的处理函数。
  const leaf = (ms) => ms.find((e) => !ms.some((x) => x !== e && e.contains(x)));
  const q1 = (root, list, exclude) => {
    for (const s of list) {
      let ms;
      try { ms = [...root.querySelectorAll(s)].filter((e) => e !== exclude && visible(e)); } catch { continue; }
      if (ms.length) return { el: leaf(ms), sel: s };
    }
    return null;
  };
  const isInput = (e) => e && (e.tagName === 'INPUT' || e.tagName === 'TEXTAREA' || e.isContentEditable);

  const findTrigger = () => {
    const hit = q1(document, sel.trigger);
    if (hit) return hit;
    // 「Search ⌘+J」这种带快捷键提示的入口。
    const ms = [...document.querySelectorAll('button,[role="button"],div,span')].filter((e) => {
      const t = (e.innerText || '').trim();
      return t.length > 0 && t.length < 30 && /搜索|Search/i.test(t) && HOTKEY_HINT.test(t) && visible(e);
    });
    return ms.length ? { el: leaf(ms), sel: 'text:hotkey-hint' } : null;
  };
  const click = (e) => {
    const r = e.getBoundingClientRect();
    const init = { bubbles: true, cancelable: true, view: window, clientX: r.left + r.width / 2, clientY: r.top + r.height / 2, button: 0 };
    for (const type of ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click']) {
      const Ev = type.startsWith('pointer') && typeof PointerEvent === 'function' ? PointerEvent : MouseEvent;
      e.dispatchEvent(new Ev(type, init));
    }
    if (typeof e.focus === 'function') e.focus();
  };
  const hotkey = () => {
    const target = document.activeElement || document.body;
    for (const mod of [{ metaKey: true }, { ctrlKey: true }]) {
      for (const type of ['keydown', 'keyup']) {
        const ev = new KeyboardEvent(type, Object.assign({ key: 'j', code: 'KeyJ', keyCode: 74, which: 74, bubbles: true, cancelable: true }, mod));
        target.dispatchEvent(ev);
      }
    }
  };
  const panelOf = (input) => {
    for (const s of sel.panel) {
      let p;
      try { p = input.closest(s); } catch { continue; }
      if (p && p !== document.body) { diag.panel = s; return p; }
    }
    let p = input;
    while (p.parentElement && p.parentElement !== document.body) p = p.parentElement;
    diag.panel = 'body-child';
    return p;
  };
  const pickInput = (exclude) => {
    // 先看焦点：唤起弹层后焦点一般会落在弹层输入框上。
    const a = document.activeElement;
    if (isInput(a) && visible(a) && a !== exclude) return { el: a, sel: 'activeElement' };
    return q1(document, sel.input, exclude);
  };
  const waitFor = async (fn, ms) => {
    const end = Date.now() + ms;
    for (;;) {
      const v = fn();
      if (v || Date.now() >= end) return v;
      await sleep(o.interval);
    }
  };

  // 1. 等应用壳子渲染出搜索入口。等了 hotkeyAfterMs 还没有入口，就按一次 ⌘/Ctrl+J。
  let pressed = false;
  const trig = await waitFor(() => {
    const hit = findTrigger() || pickInput(null);
    if (!hit && !pressed && Date.now() - t0 > o.hotkeyAfterMs) { pressed = true; hotkey(); }
    return hit;
  }, o.shellMs);
  if (!trig) return done('structure_unknown', { classes: sample(document) });

  // 2. 唤起弹层：先点入口，等不到输入框再按快捷键；入口本身就是输入框时退回用它。
  let input = null;
  if (isInput(trig.el) && !findTrigger()) {
    input = trig;
    diag.trigger = pressed ? 'hotkey' : 'input:' + trig.sel;
  } else {
    const t = findTrigger() || trig;
    diag.trigger = t.sel;
    click(t.el);
    // 入口本身可能就是个输入框：给弹层一点时间把自己的输入框挂出来，优先用弹层的。
    await sleep(600);
    input = await waitFor(() => pickInput(t.el), o.openMs);
    if (!input) {
      diag.trigger += '+hotkey';
      hotkey();
      input = await waitFor(() => pickInput(t.el), o.openMs);
    }
    if (!input && isInput(t.el)) input = { el: t.el, sel: 'trigger' };
  }
  if (!input) return done('structure_unknown', { classes: sample(document) });
  diag.inputFound = true;
  diag.input = input.sel;
  const field = input.el;
  let panel = panelOf(field);

  // 结果收集：弹层里指向飞书文档的 <a href>，以及属性值里带文档地址的元素。
  const toDoc = (raw) => {
    let u;
    try { u = new URL(raw, location.href); } catch { return null; }
    if (u.protocol !== 'https:' || !FEISHU.test(u.hostname) || !DOC_PATH.test(u.pathname)) return null;
    return u.origin + u.pathname;
  };
  const rowOf = (e) => {
    for (const s of sel.item) {
      let r;
      try { r = e.closest(s); } catch { continue; }
      if (r && panel.contains(r)) return r;
    }
    return null;
  };
  const titleOf = (e) => {
    let t = e.tagName === 'A' ? clean(e.innerText || e.textContent) : '';
    if (!t) t = clean(e.getAttribute('title') || e.getAttribute('aria-label'));
    if (!t) {
      const row = rowOf(e) || e;
      t = clean(String(row.innerText || '').split('\n').find((l) => clean(l)) || '');
    }
    return t.slice(0, 300);
  };
  const collect = () => {
    if (!panel.isConnected) {
      // 回车后弹层可能被一个结果页替换掉：重新从输入框找，找不到就看整页。
      panel = field.isConnected ? panelOf(field) : document.body;
    }
    const out = [];
    const seen = new Set();
    const anchors = [...panel.querySelectorAll('a[href]')];
    diag.anchors = anchors.length;
    for (const a of anchors) {
      const u = toDoc(a.getAttribute('href'));
      if (u && !seen.has(u)) { seen.add(u); out.push({ title: titleOf(a), url: u }); }
    }
    diag.strategy = 'anchors';
    if (!out.length) {
      const els = panel.querySelectorAll('*');
      for (let i = 0; i < els.length && i < 5000; i++) {
        const e = els[i];
        for (const at of e.attributes) {
          if (at.name === 'href' || at.name === 'style' || at.name === 'class') continue;
          const m = DOC_IN_ATTR.exec(at.value);
          const u = m && toDoc(m[0]);
          if (u && !seen.has(u)) { seen.add(u); out.push({ title: titleOf(e), url: u }); }
        }
      }
      diag.strategy = 'attributes';
    }
    diag.rows = 0;
    for (const s of sel.item) {
      let n = 0;
      try { n = [...panel.querySelectorAll(s)].filter(visible).length; } catch { /* 跳过 */ }
      if (n) { diag.rows = n; break; }
    }
    return out.slice(0, o.maxItems);
  };
  function sample(root) {
    const classes = new Set();
    for (const e of root.querySelectorAll('[class*="search" i],[class*="result" i],[class*="list" i],[class*="modal" i],[role]')) {
      const c = String(e.className && e.className.baseVal !== undefined ? e.className.baseVal : e.className || e.getAttribute('role') || '').trim();
      if (c) classes.add(c.slice(0, 120));
      if (classes.size >= 20) break;
    }
    diag.classes = [...classes];
    return diag.classes;
  }

  // 3. 填关键词前先记下弹层里原本就有的条目（「最近访问」之类），之后只认变化了的结果。
  const before = collect();
  const baseKey = before.map((i) => i.url).join('|');
  diag.baseline = before.length;
  field.focus();
  if (field.isContentEditable) {
    document.execCommand('selectAll', false);
    document.execCommand('insertText', false, o.query);
  } else {
    const proto = field.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(field, o.query);
    field.dispatchEvent(new InputEvent('input', { bubbles: true, data: o.query, inputType: 'insertText' }));
    field.dispatchEvent(new Event('change', { bubbles: true }));
  }
  diag.typed = true;
  const tTyped = Date.now();

  // 4. 等结果渲染并稳定下来。
  let items = [];
  let lastKey = '';
  let stable = 0;
  while (Date.now() - t0 < o.waitMs) {
    await sleep(o.interval);
    items = collect();
    const key = items.map((i) => i.url).join('|');
    const fresh = items.length && key !== baseKey;
    if (fresh && key === lastKey) stable++; else stable = 0;
    lastKey = key;
    if (fresh && stable >= o.stableRounds) break;
    const text = panel.innerText || '';
    if (!fresh && EMPTY.test(text) && Date.now() - tTyped > 1500) { diag.emptyHint = true; break; }
    if (!fresh && !diag.enter && Date.now() - tTyped > o.enterAfterMs && field.isConnected) {
      for (const type of ['keydown', 'keypress', 'keyup']) {
        field.dispatchEvent(new KeyboardEvent(type, { key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true, cancelable: true }));
      }
      diag.enter = true;
    }
  }
  diag.candidates = items.length;
  const fresh = items.length && items.map((i) => i.url).join('|') !== baseKey;
  if (fresh) return done('ok', { items });
  if (diag.emptyHint) return done('ok', { items: [] });
  sample(panel.isConnected ? panel : document);
  // 有结果行但读不出链接，或结果一直和填词前一样：认不出结构，宁可说不知道。
  return done('structure_unknown');
}
