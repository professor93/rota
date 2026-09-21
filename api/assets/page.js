/* Everything both of rota's pages are made of: the header they wear, the
 * credential they hold, who they turn out to be, and the sign-in they show
 * when they are nobody. The playground and the terminal are two pages on one
 * origin, so one cookie and one token box serve both — and there is one copy
 * of the code that handles them rather than two that drift apart.
 *
 * A page loads this first, fills in the three hooks below, and calls
 * pageBoot with its own name.
 */

/* ================================================================ hooks == */
// PAGE is which of the two this is. onReady is what that page does once the
// credential has been proved and the schema is in; onSignedOut is what it
// forgets; showRefusal is where a refusal is shown, which is a pane on one
// page and nothing much on the other.
let PAGE = "playground";
let onReady = async () => {};
let onSignedOut = () => {};
let showRefusal = () => {};

/* ============================================================ utilities == */
const $ = (s, r = document) => r.querySelector(s);

// el builds a node from a tag, attributes and children. Attributes that are
// null, undefined or false are skipped, so a conditional attribute is just
// an expression rather than an if.
function el(tag, attrs = {}, kids = []) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "text") n.textContent = v;
    else if (k === "html") n.innerHTML = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of [].concat(kids)) if (kid) n.append(kid);
  return n;
}
const esc = s => String(s).replace(/[&<>]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]));

// hidden is an attribute in the markup and a property in the code that reads
// it back, so both are set.
function hide(n, yes) {
  if (!n) return;
  n.hidden = yes;
  if (yes) n.setAttribute("hidden", ""); else n.removeAttribute("hidden");
}

let token = sessionStorage.getItem("rota.token") || "";

// me is who this page is, as GET /v1/session says: {name, role, via}. Null
// is signed out. The role decides what is offered, never what is allowed —
// the server decides that, and a page that hid a button would still have to
// be refused by it.
let me = null;
let schema = null;               // GET /v1/schema
let accounts = [];               // GET /v1/accounts, already in rotation order
let defaultAccount = null;       // the account a run with no id would use

/* ================================================================= http == */
async function api(method, path, body, extra = {}) {
  // No token means the cookie is the credential, and an empty bearer header
  // would be a guess the server counts against this address.
  const headers = Object.assign(token ? { authorization: "Bearer " + token } : {}, extra.headers || {});
  if (body !== undefined) headers["content-type"] = "application/json";
  const r = await fetch(path, { method, headers, credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await r.text();
  let doc = text;
  try { doc = JSON.parse(text); } catch {}
  return { ok: r.ok, status: r.status, doc };
}

function setState(text, kind) {
  const n = $("#state");
  n.textContent = text;
  n.className = "pill" + (kind ? " " + kind : "");
}

// load proves whatever credential this page is holding — a token typed in,
// or the cookie a sign-in left — with the first thing it needs anyway, so a
// wrong one says so at once and a right one costs no extra round trip.
async function load() {
  setState("connecting…", "busy");
  let first;
  try { first = await refreshAccounts(false); }
  catch { setState("unreachable", "bad"); return false; }
  if (!first.ok) {
    setState(first.status === 429 ? "blocked" : "rejected", "bad");
    showRefusal(first.doc, first.status);
    return false;
  }
  setState("connected", "ok");
  // Who this turned out to be, which a token says as much as a cookie does:
  // a token with the watch role is a watcher, and the page shows itself as
  // one rather than offering buttons the server will refuse.
  const whoami = await api("GET", "/v1/session");
  me = whoami.ok && whoami.doc && whoami.doc.role ? whoami.doc : null;
  renderWho();
  schema = (await api("GET", "/v1/schema")).doc;
  $("#ver").textContent = "v" + (schema.version || "?");
  renderLink();
  await onReady();
  return true;
}

// connect is the bearer-token way in, which is what this page had before it
// had any other.
async function connect() {
  token = $("#token").value.trim();
  if (!token) return setState("enter a token", "bad");
  if (await load()) sessionStorage.setItem("rota.token", token);
}

// signIn is the other way: a name and a password, answered with a cookie the
// browser keeps and this page never sees.
async function signIn(name, password, say) {
  if (!name || !password) return say("A name and a password.");
  setState("signing in…", "busy");
  const r = await api("POST", "/v1/session", { name, password });
  if (!r.ok) {
    setState(r.status === 429 ? "blocked" : "rejected", "bad");
    say((r.doc && r.doc.error) || "That did not work.");
    return;
  }
  // A cookie beats a token that may still be in this tab's storage: two
  // credentials on one request is one credential too many.
  token = "";
  try { sessionStorage.removeItem("rota.token"); } catch {}
  $("#token").value = "";
  await load();
}

// signOut ends the session on the server, not only in this browser.
async function signOut() {
  await api("DELETE", "/v1/session");
  me = null; token = "";
  try { sessionStorage.removeItem("rota.token"); } catch {}
  schema = null; accounts = [];
  $("#token").value = "";
  setState("signed out", "");
  renderWho(); renderLink();
  onSignedOut();
}

// watching is the one question the rest of the page asks about the role.
const watching = () => !!me && me.role === "watch";

// renderWho puts who this is in the header, and takes the token field away
// once there is a session: a page that shows both asks which one is in use.
function renderWho() {
  const box = $("#who"), out = $("#signout");
  if (!me) {
    box.replaceChildren();
    hide(box, true); hide(out, true);
    hide($("#token"), false); hide($("#connect"), false);
    return;
  }
  box.replaceChildren(
    el("b", { text: me.name }),
    el("span", { class: "role" + (watching() ? " watch" : ""), text: me.role }),
  );
  hide(box, false);
  // A bearer token is not a session to sign out of: it is on every request
  // this page makes, and the way to stop using it is to stop sending it.
  hide(out, me.via === "token");
  hide($("#token"), true); hide($("#connect"), true);
}

// lock disables every control under one node. It is how a watcher's page is
// made read-only: the server refuses these acts anyway, and this is so that
// nobody has to find that out by being refused.
function lock(root) {
  if (!watching() || !root) return;
  const walk = n => {
    if (!n) return;
    if (n.tagName === "BUTTON" || n.tagName === "INPUT" || n.tagName === "SELECT" || n.tagName === "TEXTAREA") {
      n.disabled = true;
    }
    for (const kid of Array.from(n.children || [])) walk(kid);
  };
  walk(root);
}

// watchNote is the one sentence a watcher is shown above what they cannot
// change, so a disabled button is explained rather than merely dead.
const watchNote = () => el("p", { class: "readonly",
  text: "watching: this sign-in cannot change anything" });

async function refreshAccounts(force) {
  const r = await api("GET", "/v1/accounts" + (force ? "?refresh=1" : ""));
  accounts = (r.doc && r.doc.accounts) || [];
  // The server sends them in rotation order and names the one it would
  // pick, so the page never has to work either out for itself.
  defaultAccount = (r.doc && r.doc.default) || null;
  return r;
}

const wsURL = path => location.origin.replace(/^http/, "ws") + path;

/* ============================================================ signed out == */
// viewGate is what the page is before it is anybody: a sign-in for the
// people this server's file names, and under it the bearer token, which is
// how this page has always been used and still is.
function viewGate(panel) {
  const name = el("input", { class: "ctl", id: "signin_name", autocomplete: "username",
    placeholder: "name", spellcheck: "false" });
  const pass = el("input", { class: "ctl", id: "signin_password", type: "password",
    autocomplete: "current-password", placeholder: "password" });
  const said = el("p", { class: "help", style: "margin:9px 0 0", text: "" });
  const say = msg => { said.textContent = msg; };
  const go = () => signIn(name.value.trim(), pass.value, say);
  for (const box of [name, pass]) {
    box.addEventListener("keydown", e => { if (e.key === "Enter") { e.preventDefault(); go(); } });
  }
  panel.append(
    el("div", { class: "step" }, [
      el("h3", {}, [el("i", { text: "1" }), el("span", { text: "Sign in" })]),
      el("p", { class: "help", text: "The name and password this server was given in its file. A sign-in lasts until it expires or you sign out; nothing is kept in this browser but the cookie." }),
      el("div", { class: "row2" }, [
        el("div", { class: "field" }, [el("label", { class: "flabel", for: "signin_name", text: "Name" }), name]),
        el("div", { class: "field" }, [el("label", { class: "flabel", for: "signin_password", text: "Password" }), pass]),
      ]),
      el("div", { style: "margin-top:9px" }, [
        el("button", { class: "btn primary", id: "signin", text: "Sign in", onclick: go }),
      ]),
      said,
    ]),
    el("div", { class: "step" }, [
      el("h3", {}, [el("i", { text: "2" }), el("span", { text: "Or use a bearer token" })]),
      el("p", { class: "help", text: "The token this server was started with, or one its file names. Put it in the box at the top of the page and press Connect; it is kept in this tab and nowhere else." }),
    ]),
  );
}

/* =============================================================== header == */
// The header is the same strip on both pages, so it is written once here
// rather than kept in step in two files by hand.
const HEADER = `
  <div class="brand">rota <span id="pagename"></span></div>
  <span id="ver" class="ver"></span>
  <a id="otherpage" class="btn ghost" hidden></a>
  <span class="grow"></span>
  <span id="who" class="who" hidden></span>
  <button id="signout" class="btn ghost" hidden>Sign out</button>
  <label class="sr" for="token">Bearer token</label>
  <input id="token" type="password" placeholder="bearer token" autocomplete="off" spellcheck="false">
  <button id="connect" class="btn">Connect</button>
  <span id="state" class="pill" role="status" aria-live="polite">not connected</span>
  <button id="theme" class="btn icon" title="Switch light and dark" aria-label="Switch light and dark">◐</button>`;

// renderLink offers the other page, and only where the server is serving it:
// a link to a route group that is off is a link to a 404.
function renderLink() {
  const a = $("#otherpage");
  if (!a) return;
  const to = PAGE === "terminal" ? "playground" : "terminal";
  const on = !!schema && (to === "terminal" ? schema.terminal === true : schema.playground === true);
  hide(a, !on);
  if (!on) return;
  a.setAttribute("href", "/" + to);
  a.textContent = to === "terminal" ? "Terminal →" : "← Playground";
}

/* ================================================================= boot == */
const THEME_KEY = "rota.theme";
function applyTheme(t) {
  try { localStorage.setItem(THEME_KEY, t); } catch {}
  const root = document.documentElement;
  if (!root) return;
  if (t === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", t);
}
function cycleTheme() {
  let now = "system";
  try { now = localStorage.getItem(THEME_KEY) || "system"; } catch {}
  applyTheme({ system: "light", light: "dark", dark: "system" }[now]);
}
// pageBoot is the last line of either page: the header, the theme, the
// keyboard, and then the question of who this is.
function pageBoot(name) {
  PAGE = name;
  const top = $("#top");
  if (top) top.innerHTML = HEADER;
  $("#pagename").textContent = name;
  try { applyTheme(localStorage.getItem(THEME_KEY) || "system"); } catch {}
  $("#connect").addEventListener("click", connect);
  $("#signout").addEventListener("click", signOut);
  $("#theme").addEventListener("click", cycleTheme);
  $("#token").addEventListener("keydown", e => { if (e.key === "Enter") connect(); });
  $("#token").value = token;
  renderWho();
  renderLink();
  return boot();
}

// boot asks who this page is before anything else. A session says so and the
// page carries on signed in; nothing says so and it shows the sign-in, unless
// this tab is still holding a token from earlier.
async function boot() {
  const whoami = await api("GET", "/v1/session");
  if (whoami.ok && whoami.doc && whoami.doc.role) {
    me = whoami.doc;
    renderWho();
    await load();
    return;
  }
  me = null;
  renderWho();
  if (token) await connect();
}
