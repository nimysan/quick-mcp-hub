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

  // 私有文档会跳去登录页；那不是我们该处理的页面。
  if (!/(\.feishu\.cn|\.larksuite\.com|\.larkoffice\.com)$/.test(location.hostname) ||
      !/^\/(docx|wiki)\//.test(location.pathname)) {
    return { status: 'error', error: '页面跳转到了 ' + location.hostname + location.pathname.slice(0, 40) + '（可能没登录或没有权限）', diag };
  }

  // 1. 等引擎。只看「有没有」，不读内容。
  let pm = null;
  while (Date.now() - t0 < o.waitMs) {
    pm = window.PageMain;
    if (pm && pm.blockManager && pm.blockManager.rootBlockModel) break;
    pm = null;
    await sleep(300);
  }
  diag.waitedMs = Date.now() - t0;
  if (!pm) {
    // 知识库里也可能挂着电子表格、多维表格：没有 #mainBox 说明不是 docx 编辑器。
    return { status: document.querySelector('#mainBox') ? 'engine_missing' : 'unsupported_type', diag };
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
