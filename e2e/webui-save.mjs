#!/usr/bin/env node
// 配置页详情页「账号配置」「权限与工具」两个标签页保存时互不覆盖的端到端检查。
//
// 用真的页面（无头 Chromium 通过 CDP 点按钮），不是重写一遍 keepOther 的逻辑。
// 被测程序以 `-ui` 起在 127.0.0.1，临时 HOME，不碰用户真实配置，不写钥匙串：
//   - 本机邮件（无凭据）：真的保存，再读临时 HOME 里的 channels.json 核对；
//   - 企业邮箱（有凭据）：保存要求钥匙串里已有密码，所以在页面里截下 POST 的请求体
//     核对它带没带上另一页的设置，并假装保存成功（请求不发到服务端）。
//
// 用法：
//   node e2e/webui-save.mjs <mcp-hub 二进制> <chromium 可执行文件> [--expect-bug]
// 环境变量：
//   E2E_TMP          临时目录的父目录（默认 os.tmpdir()）
//   E2E_APPLE_DIR    本机邮件夹具用的 Apple Mail 目录（测试并保存要求能找到数据）
//   E2E_LEGACY_DIR   旧版 Outlook 的 Data 目录
// --expect-bug：对旧版（main）二进制跑，只跑企业邮箱那段，期望复现「另一页设置丢失」。
// 只输出字段名和真假，不输出任何邮件内容（这里也不读邮件，只列文件夹数）。
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import net from "node:net";

const [bin, chrome, flag] = process.argv.slice(2);
const expectBug = flag === "--expect-bug";
if (!bin || !chrome) {
  console.error("用法: node e2e/webui-save.mjs <mcp-hub> <chromium> [--expect-bug]");
  process.exit(2);
}
const base = process.env.E2E_TMP || os.tmpdir();
const work = fs.mkdtempSync(path.join(base, "webui-e2e-"));
const home = path.join(work, "home");
const cfgDir = path.join(home, "Library", "Application Support", "mcp-hub");
const cfgFile = path.join(cfgDir, "channels.json");
fs.mkdirSync(cfgDir, { recursive: true });

const realHome = os.homedir();
const appleDir = process.env.E2E_APPLE_DIR || path.join(realHome, "Library", "Mail");
const legacyDir = process.env.E2E_LEGACY_DIR || path.join(realHome, "Library", "Group Containers",
  "UBF8T346G9.Office", "Outlook", "Outlook 15 Profiles", "Main Profile", "Data");

const LOCAL = "本机";
const LOCAL_OFF = "本机-来源全关";
const MAIL = "e2e-fake@example.invalid";
const seed = [
  { type: "custommail", account: MAIL, enabled: true, settings: {
    allowSend: "false", imapHost: "imap.example.invalid", imapPort: "993",
    smtpHost: "smtp.example.invalid", smtpPort: "465" } },
];
if (!expectBug) {
  seed.push({ type: "localmail", account: LOCAL, enabled: true, settings: {
    name: LOCAL, apple_mail: "true", outlook_legacy: "true", outlook: "false",
    apple_mail_dir: appleDir, outlook_legacy_dir: legacyDir,
    maxBodyBytes: "10000", maxResults: "30" } });
  // 来源全关的本机邮件：服务不会注册任何工具，配置页也不能说「已启用」（L3）。
  seed.push({ type: "localmail", account: LOCAL_OFF, enabled: true, settings: {
    name: LOCAL_OFF, apple_mail: "false", outlook_legacy: "false", outlook: "false" } });
}
fs.writeFileSync(cfgFile, JSON.stringify({ channels: seed }));

const freePort = () => new Promise(r => { const s = net.createServer(); s.listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => r(p)); }); });
const sleep = ms => new Promise(r => setTimeout(r, ms));
const procs = [];
let failed = 0;
const check = (ok, what) => { console.log(`${ok ? "PASS" : "FAIL"} ${what}`); if (!ok) failed++; };

async function waitHTTP(url, ms = 15000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { const r = await fetch(url); if (r.ok) return r; } catch {}
    await sleep(150);
  }
  throw new Error("等不到 " + url);
}

// ---------- CDP ----------
let ws, seq = 0;
const pending = new Map();
function cdp(method, params = {}) {
  const id = ++seq;
  ws.send(JSON.stringify({ id, method, params }));
  return new Promise((res, rej) => pending.set(id, { res, rej }));
}
async function evaluate(expr) {
  const r = await cdp("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error("页面脚本出错: " + JSON.stringify(r.exceptionDetails.exception?.description || r.exceptionDetails.text));
  return r.result.value;
}
async function waitFor(expr, ms = 20000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (await evaluate(`!!(${expr})`)) return;
    await sleep(100);
  }
  throw new Error("等不到页面条件: " + expr);
}
const savedSettings = (type, account) =>
  (JSON.parse(fs.readFileSync(cfgFile, "utf8")).channels.find(c => c.type === type && c.account === account) || {}).settings || {};

// 页面里截 POST /api/channels(/test)：记下请求体，假装成功。只给企业邮箱用。
const INTERCEPT = `(() => {
  if (window.__cap) return;
  window.__cap = [];
  const orig = window.fetch;
  window.fetch = (url, opt) => {
    if (opt && opt.method === "POST" && (url === "/api/channels" || url === "/api/channels/test")) {
      window.__cap.push({ url, body: JSON.parse(opt.body) });
      return Promise.resolve(new Response(JSON.stringify({ ok: true }), { headers: { "Content-Type": "application/json" } }));
    }
    return orig(url, opt);
  };
})()`;

async function main() {
  const uiPort = await freePort(), dbgPort = await freePort();
  const hub = spawn(bin, ["-ui", "-no-browser", "-ui-port", String(uiPort)], {
    env: { HOME: home, TMPDIR: work, PATH: process.env.PATH, USER: process.env.USER, LANG: "en_US.UTF-8" },
    stdio: ["ignore", "ignore", "ignore"],
  });
  procs.push(hub);
  const origin = `http://127.0.0.1:${uiPort}`;
  await waitHTTP(origin + "/healthz");
  // 只绑 127.0.0.1
  const lsof = await new Promise(r => { const p = spawn("lsof", ["-nP", "-a", "-p", String(hub.pid), "-iTCP", "-sTCP:LISTEN"]); let o = ""; p.stdout.on("data", d => o += d); p.on("close", () => r(o)); });
  const listens = lsof.split("\n").filter(l => l.includes("LISTEN"));
  check(listens.length > 0 && listens.every(l => l.includes("127.0.0.1:")), `配置页只监听 127.0.0.1（${listens.length} 个监听）`);

  const br = spawn(chrome, ["--headless", `--remote-debugging-port=${dbgPort}`, "--remote-debugging-address=127.0.0.1",
    `--user-data-dir=${path.join(work, "chrome")}`, "--no-first-run", "--no-default-browser-check", "about:blank"],
    { stdio: ["ignore", "ignore", "ignore"] });
  procs.push(br);
  await waitHTTP(`http://127.0.0.1:${dbgPort}/json/version`);
  const targets = await (await fetch(`http://127.0.0.1:${dbgPort}/json/list`)).json();
  const page = targets.find(t => t.type === "page");
  ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.rej(new Error(m.error.message)) : p.res(m.result); }
  };
  await cdp("Runtime.enable");
  await cdp("Page.enable");

  const open = async (type, account, tab) => {
    await cdp("Page.navigate", { url: `${origin}/#/c/${encodeURIComponent(type)}/${encodeURIComponent(account)}/${tab}` });
    await sleep(300);
    await waitFor(tab === "account" ? `document.getElementById("dsave")` : `document.getElementById("dsave2")`);
  };
  const waitSaved = async () => {
    await waitFor(`(()=>{const s=document.getElementById("dst"); return s && /已保存|失败|连不上/.test(s.textContent)})() || document.querySelector(".toast")`, 60000);
    await sleep(300);
  };

  // ---------- 本机邮件：真保存 ----------
  if (!expectBug) {
    await open("localmail", LOCAL, "account");
    await evaluate(`(()=>{const e=document.getElementById("df_maxBodyBytes"); e.value="4096"; e.dispatchEvent(new Event("input"));})()`);
    await evaluate(`document.getElementById("dsave").click()`);
    await waitSaved();
    let s = savedSettings("localmail", LOCAL);
    check(s.maxBodyBytes === "4096", "本机邮件·账号配置页保存：新正文上限已写入");
    check(s.apple_mail === "true" && s.outlook_legacy === "true", "本机邮件·账号配置页保存：权限页的来源开关没被关掉");
    check(s.outlook_legacy_dir === legacyDir && s.apple_mail_dir === appleDir && s.maxResults === "30", "本机邮件·账号配置页保存：其他文本设置保留");

    await open("localmail", LOCAL, "perm");
    await evaluate(`(()=>{const c=document.querySelector('[data-sw="outlook"]'); c.checked=true; c.dispatchEvent(new Event("change"));})()`);
    await evaluate(`document.getElementById("dsave2").click()`);
    await waitSaved();
    s = savedSettings("localmail", LOCAL);
    check(s.outlook === "true" && s.apple_mail === "true" && s.outlook_legacy === "true", "本机邮件·权限页保存：来源开关按页面状态写入");
    check(s.maxBodyBytes === "4096" && s.maxResults === "30", "本机邮件·权限页保存：账号配置页的数值设置保留");
    check(s.outlook_legacy_dir === legacyDir && s.apple_mail_dir === appleDir, "本机邮件·权限页保存：自定义目录保留");

    // 再关一个来源，确认「关」也能存下来（keepOther 不该把已存的 true 带回来）
    await open("localmail", LOCAL, "perm");
    await evaluate(`(()=>{const c=document.querySelector('[data-sw="apple_mail"]'); c.checked=false; c.dispatchEvent(new Event("change"));})()`);
    await evaluate(`document.getElementById("dsave2").click()`);
    await waitSaved();
    s = savedSettings("localmail", LOCAL);
    check(s.apple_mail === "false" && s.outlook === "true", "本机邮件·权限页保存：关掉的来源确实关了");

    // ---------- L3：本机邮件的文案和工具数 ----------
    await open("localmail", LOCAL, "account");
    const acc = await evaluate(`document.getElementById("dmain").textContent`);
    check(!/服务器设置|SSL|TLS/.test(acc) && acc.includes("高级设置"), "本机邮件·账号配置页：高级字段标题不提服务器和 SSL / TLS");
    check(!acc.includes("凭据") && acc.includes("不需要密码"), "本机邮件·账号配置页：保存栏不提凭据");

    await open("localmail", LOCAL_OFF, "perm");
    const offPills = await evaluate(`document.querySelectorAll("#dmain .tools .pill.ok").length`);
    const perm = await evaluate(`document.getElementById("dmain").innerText`);
    check(offPills === 0 && perm.includes("0 个已启用"), `本机邮件·来源全关：权限页显示 0 个工具已启用（标「已启用」的 ${offPills} 个）`);
    await open("localmail", LOCAL, "perm");
    const onPills = await evaluate(`document.querySelectorAll("#dmain .tools .pill.ok").length`);
    check(onPills === 3, `本机邮件·开了来源：权限页 3 个工具已启用（实际 ${onPills} 个）`);
  }

  // ---------- L3：企业邮箱的文案不受影响 ----------
  if (!expectBug) {
    await open("custommail", MAIL, "account");
    const acc = await evaluate(`document.getElementById("dmain").textContent`);
    // 只有本机邮件有高级字段，其他渠道本来就没有「服务器设置」这一块；这里确认保存栏文案没变、也没多出新块。
    check(acc.includes("凭据保存在本机") && !acc.includes("高级设置") && !acc.includes("不需要密码"),
      "企业邮箱·账号配置页：保存栏凭据文案保持原样，没有多出本机邮件的文案");
  }

  // ---------- 企业邮箱：截请求体 ----------
  await open("custommail", MAIL, "perm");
  await evaluate(INTERCEPT);
  await evaluate(`(()=>{const c=document.querySelector('[data-sw="allowSend"]'); c.checked=true; c.dispatchEvent(new Event("change"));})()`);
  await evaluate(`document.getElementById("dsave2").click()`);
  await waitFor(`window.__cap.some(x=>x.url==="/api/channels")`);
  let body = (await evaluate(`window.__cap.find(x=>x.url==="/api/channels").body`)).values;
  const keptHosts = body.imapHost === "imap.example.invalid" && body.smtpHost === "smtp.example.invalid";
  if (expectBug) {
    check(!keptHosts, "旧版·企业邮箱权限页保存：复现了「主机设置没带上」（应为 FAIL 才说明修了个真 bug）");
  } else {
    check(body.allowSend === "true", "企业邮箱·权限页保存：开关值正确");
    check(keptHosts && body.imapPort === "993", "企业邮箱·权限页保存：请求带上了账号配置页的主机和端口");
    check(!("password" in body), "企业邮箱·权限页保存：请求里没有密码字段");

    await open("custommail", MAIL, "account");
    await evaluate(INTERCEPT);
    await evaluate(`window.__cap.length = 0`);
    await evaluate(`document.getElementById("dsave").click()`);
    await waitFor(`window.__cap.some(x=>x.url==="/api/channels")`);
    body = (await evaluate(`window.__cap.find(x=>x.url==="/api/channels").body`)).values;
    // 页面截下的 POST 没发到服务端，所以已存的 allowSend 仍是 false
    check(body.allowSend === "false" && body.imapHost === "imap.example.invalid", "企业邮箱·账号配置页保存：请求带上了权限页的开关");
  }
}

try {
  await main();
} catch (e) {
  console.log("FAIL 脚本出错: " + e.message);
  failed++;
} finally {
  try { ws && ws.close(); } catch {}
  for (const p of procs) { try { p.kill("SIGTERM"); } catch {} }
  await sleep(800);
  fs.rmSync(work, { recursive: true, force: true });
}
console.log(failed ? `${failed} 项失败` : "全部通过");
process.exit(failed ? 1 : 0);
