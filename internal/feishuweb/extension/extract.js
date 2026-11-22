// 页面内提取脚本。background.js 用 chrome.scripting.executeScript({world:'MAIN', func: feishuExtract})
// 把它注入飞书文档页面的主世界执行：只有主世界能看到 window.PageMain。
//
// 这个函数会被序列化后注入，所以必须自包含：不能引用外面的任何变量。
//
// 顺序是硬性要求（feishu-auto-verify.md §3b）：**判定在先、读取在后**。
// 只有 PageMain.props.copyable === true && exportable === true &&
// isForceSetSecurity !== true 才读；否则直接返回 refused，不碰块树。
// mcp-hub 侧还会用同一套规则再判一次（feishuweb.Gate）。
//
// 只读页面内存里已经加载的内容：不发任何网络请求、不读 cookie、不调飞书的任何内部接口。
//
// 块树读取点和就绪判定参考 cloud-document-converter（MIT，
// https://github.com/whale4113/cloud-document-converter ）的 docx.ts。
/* exported feishuExtract */
async function feishuExtract(opts) {
  const o = Object.assign({ waitMs: 15000, maxRounds: 100, interval: 400, maxBlocks: 20000 }, opts || {});
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const t0 = Date.now();
  const diag = {
    at: new Date().toISOString(), host: location.host, pageMain: false,
    flags: { copyable: null, exportable: null, isForceSetSecurity: null },
    topChildren: 0, pendingBefore: 0, pendingAfter: 0, scrollRounds: 0, scrollHeight: 0, waitedMs: 0,
  };

  // 页面去向：登录页、404 页、无权限页要尽早认出来，不要干等引擎超时（用户实测假 token
  // 要等 18~20 秒才回，还被误判成 unsupported_type）。只在还没有 PageMain 时看页面文字，
  // 并且只看短页面：正文里出现「没有权限」之类的字不能算数。
  const startPath = location.pathname;
  const NOT_FOUND = /文档不存在|页面不存在|内容不存在|已被删除|已删除|已被移除|链接已失效|找不到(该|这个)?(文档|页面)|does ?n[o']t exist|not found|has been deleted|was deleted|been removed|page isn['’]t available|\b404\b/i;
  const NO_PERM = /没有(访问|查看|阅读)?权限|无(访问|查看|阅读)?权限|权限不足|申请(访问)?权限|需要权限|no (access|permission)|don['’]t have (access|permission)|request (access|permission)|permission denied|access denied/i;
  const STATE_BOX = '[class*="empty" i],[class*="error" i],[class*="not-found" i],[class*="notfound" i],[class*="404"],' +
    '[class*="permission" i],[class*="no-auth" i],[class*="noauth" i],[class*="apply" i],[class*="exception" i]';
  const where = () => {
    const h = location.hostname;
    if (/^(accounts|passport)\./.test(h) || /\/(login|passport)/.test(location.pathname)) return 'login_required';
    if (!/(\.feishu\.cn|\.larksuite\.com|\.larkoffice\.com)$/.test(h) || /^www\./.test(h) || /^\/404/.test(location.pathname)) return 'not_found';
    if (!/^\/(docx|wiki)\//.test(location.pathname)) return 'moved';
    // 编辑器外壳已经出来了就不看文字：那是一篇正在加载的正常文档。
    if (document.querySelector('#mainBox')) return '';
    // 只看错误 / 空状态容器里的字；整页字很少（< 300）时才看整页。知识库侧栏目录里
    // 出现「404」之类的标题不能算数。
    const body = document.body ? String(document.body.innerText || '') : '';
    let text = body.length < 300 ? body : '';
    if (!text) {
      for (const e of document.querySelectorAll(STATE_BOX)) text += ' ' + String(e.innerText || '').slice(0, 500);
    }
    if (NO_PERM.test(text)) return 'no_permission';
    if (NOT_FOUND.test(text)) return 'not_found';
    return '';
  };
  const shellMs = o.shellMs || 7000;
  const sheetLike = () => !!document.querySelector('[class*="spreadsheet" i],[class*="bitable" i],[class*="sheet-container" i],[class*="mindnote" i],[class*="slides" i]');

  // 1. 等引擎。只看「有没有」，不读内容。
  let pm = null;
  let gone = '';
  while (Date.now() - t0 < o.waitMs) {
    pm = window.PageMain;
    if (pm && pm.blockManager && pm.blockManager.rootBlockModel) break;
    pm = null;
    // 同一个判断连续两次（间隔 300ms 以上）才算数，免得撞上加载中途的过渡页面。
    const w = where();
    if (w && w === gone) break;
    gone = w;
    // 连编辑器外壳（#mainBox）都没有、也不是表格类页面，等够 shellMs 就不再等了。
    if (Date.now() - t0 > shellMs && !document.querySelector('#mainBox') && !sheetLike()) break;
    await sleep(300);
  }
  diag.waitedMs = Date.now() - t0;
  diag.path = location.pathname.slice(0, 80);
  if (!pm) {
    const page = location.hostname + location.pathname.slice(0, 40);
    if (gone === 'login_required') return { status: 'login_required', url: location.origin + location.pathname, diag };
    if (gone === 'no_permission') return { status: 'no_permission', diag };
    if (gone === 'not_found') return { status: 'not_found', error: '页面显示文档不存在（' + page + '）', diag };
    if (gone === 'moved') return { status: 'not_found', error: '页面跳转到了 ' + page + '（文档可能不存在）', diag };
    // 知识库里也可能挂着电子表格、多维表格。
    if (sheetLike()) return { status: 'unsupported_type', diag };
    // 有编辑器外壳却等不到引擎：飞书前端可能改版了。
    if (document.querySelector('#mainBox')) return { status: 'engine_missing', diag };
    // 地址本来就是 docx / wiki，页面上却什么都没有：按「不存在或打不开」处理，
    // 不再说成「不是云文档」。
    return { status: 'not_found', error: '页面上没有文档内容（' + startPath.slice(0, 40) + '）', diag };
  }
  diag.pageMain = true;

  // 2. 判定。字段缺失或不是布尔值记成 null，按 permission_unknown 拒读。
  const props = pm.props || {};
  const asBool = (v) => (typeof v === 'boolean' ? v : null);
  const flags = {
    copyable: asBool(props.copyable),
    exportable: asBool(props.exportable),
    isForceSetSecurity: asBool(props.isForceSetSecurity),
  };
  diag.flags = flags;
  let reason = '';
  if (flags.copyable === null || flags.exportable === null) reason = 'permission_unknown';
  else if (flags.copyable !== true) reason = 'copy_restricted';
  else if (flags.exportable !== true) reason = 'export_restricted';
  else if (flags.isForceSetSecurity === true) reason = 'security_restricted';
  if (reason) {
    return { status: 'refused', reason, flags, diag };
  }

  // 3. 放行之后才读。滚动只用来触发懒加载，内容从模型里读。
  const root = pm.blockManager.rootBlockModel;
  const kids = () => (Array.isArray(root.children) ? root.children : []);
  const pendingCount = () => kids().filter((c) =>
    !c || !c.snapshot || c.snapshot.type === 'pending' ||
    (c.type === 'synced_reference' && !c.isAllDataReady)).length;
  diag.topChildren = kids().length;
  diag.pendingBefore = pendingCount();
  const sc = document.querySelector('#mainBox .bear-web-x-container');
  if (sc) {
    const top0 = sc.scrollTop;
    let rounds = 0;
    while (pendingCount() > 0 && rounds < o.maxRounds) {
      sc.scrollTo({ top: sc.scrollHeight });
      await sleep(o.interval);
      rounds++;
    }
    diag.scrollRounds = rounds;
    diag.scrollHeight = sc.scrollHeight;
    sc.scrollTo({ top: top0 });
  }
  diag.pendingAfter = pendingCount();

  // 4. 抄块树：只抄转换要用的字段。
  const KEEP = ['bold', 'italic', 'strikethrough', 'underline', 'inlineCode', 'equation', 'link', 'fixEnter', 'inline-component'];
  const str = (v) => (v === undefined || v === null ? '' : String(v));
  const pickOps = (z) => {
    const ops = z && z.content && z.content.ops;
    if (!Array.isArray(ops)) return undefined;
    return ops.map((op) => {
      const a = {};
      if (op && op.attributes) for (const k of KEEP) if (op.attributes[k] !== undefined) a[k] = str(op.attributes[k]);
      return Object.keys(a).length ? { insert: str(op && op.insert), attributes: a } : { insert: str(op && op.insert) };
    });
  };
  let count = 0;
  const ser = (blk, depth) => {
    if (!blk || count >= o.maxBlocks || depth > 64) return null;
    count++;
    const s = blk.snapshot || {};
    const type = str(blk.type || s.type);
    const out = { type };
    const ops = pickOps(blk.zoneState);
    if (ops && ops.length) out.ops = ops;
    if (type === 'code') {
      out.text = str(blk.zoneState && blk.zoneState.allText);
      out.language = str(blk.language || s.language);
    }
    if (/^heading[1-9]$/.test(type)) {
      if (typeof s.seq === 'string') out.seq = s.seq;
      if (typeof s.seq_level === 'string') out.seqLevel = s.seq_level;
    }
    if (type === 'ordered' && s.seq !== undefined) out.seq = str(s.seq);
    if (type === 'todo') out.done = !!s.done;
    if (type === 'image' && s.image) {
      out.name = str(s.image.name);
      const cap = s.image.caption && s.image.caption.text && s.image.caption.text.initialAttributedTexts &&
        s.image.caption.text.initialAttributedTexts.text;
      if (cap && cap[0]) out.caption = str(cap[0]);
    }
    if (type === 'file' && s.file) out.name = str(s.file.name);
    if (type === 'table') out.columns = Array.isArray(s.columns_id) ? s.columns_id.length : 0;
    if (type === 'iframe') out.url = str(s.iframe && s.iframe.component && s.iframe.component.url);
    if (type === 'isv' && s.block_type_id === 'blk_631fefbbae02400430b8f9f4') out.mermaid = str(s.data && s.data.data);
    let children = blk.children;
    if (type === 'synced_reference' && blk.innerBlockManager && blk.innerBlockManager.rootBlockModel) {
      children = blk.innerBlockManager.rootBlockModel.children;
    }
    if (Array.isArray(children) && children.length) {
      out.children = children.map((c) => ser(c, depth + 1)).filter(Boolean);
    }
    return out;
  };
  const tree = ser(root, 0);
  const title = str(root.zoneState && root.zoneState.allText).replace(/\n$/, '') || document.title;
  return { status: 'ok', title, url: location.href, flags, diag, root: tree };
}
