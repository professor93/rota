"use strict";

/* A terminal that lives in the server, drawn here.
 *
 * Three regions: the terminals this server holds on the left, the terminal
 * itself in the middle, and what is true about it on the right. Everything
 * the middle and the right show after the first frame comes from the socket
 * — a page that told you who was typing a second ago is a page that lied.
 *
 * xterm.js is named in exactly one place, makeTerminal in view.js, and
 * reached through the small object it returns. That is so a test can run
 * this code with a terminal that only records what it was shown, and so the
 * emulator could be replaced one day without reading the rest of this.
 *
 * This file is the state, choosing a terminal, the socket, its frames, the
 * keyboard and the strip. The page's other regions are files of their own
 * under /assets/term/, loaded in a fixed order, with boot.js last.
 */

// The clock, in one place, so a ten-second countdown can be tested without
// waiting ten seconds for it.
let termNow = () => Date.now();
let termLater = (fn, ms) => setTimeout(fn, ms);

/* ================================================================ state == */
let terms = [];              // GET /v1/terminals, live first
let termId = null;           // the one that is open
let termGen = 0;             // which selection is current, across awaits
let termInfo = null;         // its description, for the things frames never say
let termSock = null;         // the one socket; there is never a second
let termView = null;         // the emulator adapter
let termSeen = false;        // a hello has arrived for this terminal
let termOffset = 0;          // the absolute offset of the next byte
let termPaint = false;       // the next binary frame is a held session's screen
let termCols = 80, termRows = 24;
let termKind = null;         // "account", "shell", or "shared"
let termMode = null;         // for a shared one: "control" or "watch"
let termHolder = null;       // who holds the keyboard, null for nobody
let termYou = null;          // {name, role, conn} from hello
let termViewers = [];
let termEnded = null;        // the exit code, once there is one
let termStatus = "idle";     // idle | connecting | live | reconnecting | ended
let termWait = 0;            // seconds until the next attempt
let termTries = 0;
let termClaimAt = null;      // when this tab asked for the keyboard
let termAsking = null;       // {by, id}: somebody asking this tab for it
let termNote = "";           // the last thing the server refused
let termNudge = false;       // a key pressed by somebody who may not type
let termOpenNew = false;     // the New terminal form is unfolded
let termPolling = false;
const CLAIM_WAIT = 10000;    // the server's own wait before a force is allowed
const TERM_BACKOFF = [1, 2, 4, 8, 15];

const termWatcher = () => !!termYou && termYou.role === "watch";
// A shared terminal is one somebody is sitting at, offered to this server by
// the rota running it. Two things follow, and both are facts about that
// terminal rather than about this page: its size belongs to the window it is
// on, and it may have been offered to be read and never typed into.
const termShared = () => termKind === "shared";
const termLookOnly = () => termShared() && termMode === "watch";
// termIHold: the keyboard is held by a name, and hello said which name this
// connection has. Two sockets of one principal share a name, which is as
// close as the server gets to "you" — and either way it is one person.
const termIHold = () => !!termYou && !termWatcher() && !termLookOnly() &&
  termEnded === null && termHolder === termYou.name;

// A held session says more than a name about its keyboard: which connection
// holds it. Nothing reads these until a held session's parts of the page do.
let termHeld = false;       // from the description's held, or hello's holder_conn
let termHolderConn = null;  // which conn holds a held session's keyboard
let termState = null;       // {status, waiting_for, title, name}; Task 3 fills it
const termHoldsHere = () => termIHold() && (!termHeld || termHolderConn === (termYou && termYou.conn));
const termCanType = () => !!termYou && !watching() && !termWatcher() && !termLookOnly() && termEnded === null;
const termWaiting = () => !!((termState && termState.waiting_for) || (termInfo && termInfo.status === "needs_you"));

/* ============================================================== the page == */
// enterTerminal is this page once there is a credential: ask what there is,
// fetch the emulator, and keep asking gently while somebody is looking.
function enterTerminal() {
  hide($("#termshell"), false);
  $("#gate").replaceChildren();
  loadXterm().catch(() => termSay("the terminal emulator could not be loaded"));
  refreshTerminals();
  renderTerm();
  // A terminal this page had open is picked up from where it was shown to,
  // rather than started again from the top.
  if (termId && !termSock && termEnded === null) termConnect();
  if (!termPolling) { termPolling = true; termPoll(); }
  termRouteBoot();
  termEvents();
}

// The gate is this page with nobody behind it: the same sign-in the
// playground shows, because it is the same sign-in.
function renderGate() {
  hide($("#termshell"), true);
  const gate = $("#gate");
  gate.replaceChildren();
  viewGate(gate);
}

function termPoll() {
  if (!schema) { termPolling = false; return; }
  if (!document.hidden) refreshTerminals();
  termLater(termPoll, 10000);
}

/* ========================================================= what there is == */
async function refreshTerminals() {
  const r = await api("GET", "/v1/terminals");
  const list = (r.doc && r.doc.terminals) || [];
  terms = termOrder(list);
  renderTermList();
  // A terminal open on this page keeps its description up to date from the
  // listing too, for the few things no frame carries.
  const mine = terms.find(t => t.id === termId);
  if (mine) { termInfo = mine; renderTermInfo(); }
}

/* =================================================== choosing and attaching == */
async function termSelect(id) {
  if (id === termId) return;
  termDetach();
  termDrop();
  const mine = ++termGen;
  termId = id;
  termInfo = terms.find(t => t.id === id) || null;
  termSeen = false; termOffset = 0; termTries = 0; termWait = 0;
  termHolder = null; termYou = null; termViewers = []; termEnded = null;
  termAsking = null; termClaimAt = null; termNote = "";
  termCols = 80; termRows = 24;
  termKind = null; termMode = null;
  termHeld = false; termHolderConn = null; termState = null;
  termStatus = id ? "connecting" : "idle";
  renderTerm();
  if (!id) { termRoute(null); return; }
  // The description carries what no frame does: the account, the folder, when
  // it started, whether it is being recorded, and when the login inside it
  // lapses. After this the panel lives on frames alone.
  const r = await api("GET", "/v1/terminals/" + encodeURIComponent(id));
  if (mine !== termGen) return;
  if (r.ok) termInfo = r.doc;
  termHeld = !!(termInfo && termInfo.held);
  termRoute(id);
  renderTermInfo();
  await termScreen();
  if (mine !== termGen) return;
  termConnect();
}

// termScreen makes the emulator, once, for the terminal now open.
async function termScreen() {
  const host = $("#tscreen");
  if (!host || termView) return;
  host.replaceChildren();
  host.classList.remove("nothing");
  const mine = termGen;
  let view;
  try { view = await makeTerminal(host); }
  catch (e) { termSay("the terminal could not be drawn: " + (e && e.message ? e.message : e)); return; }
  if (mine !== termGen) { view.dispose(); return; }
  termView = view;
  // Typing is sent while this page holds the keyboard and swallowed while it
  // does not: the server refuses it either way, and a page that sent every
  // keystroke to be refused would be asking to be told off for each one.
  view.onData(data => {
    if (!termIHold() || !termSock) return termFlash();
    try { termSock.send(new TextEncoder().encode(data)); } catch {}
  });
  termFit();
}

function termDrop() {
  if (!termView) return;
  try { termView.dispose(); } catch {}
  termView = null;
  const host = $("#tscreen");
  if (host) host.replaceChildren();
}

// termDetach closes this page's socket for good: a clean 1000, which is a
// person leaving rather than a connection breaking.
function termDetach() {
  const sock = termSock;
  termSock = null;
  if (!sock) return;
  try { sock.close(1000); } catch {}
}

// termConnect opens the one socket. Saying nothing about since replays the
// whole scrollback, which is what a page opening a terminal wants; after a
// hello it is always the offset this page has actually been shown.
function termConnect() {
  if (termSock || !termId || termEnded !== null) return;
  const q = termSeen ? "?since=" + termOffset : "";
  const url = wsURL("/v1/terminals/" + encodeURIComponent(termId) + "/ws" + q);
  // A browser cannot put a header on a WebSocket, so a token travels as a
  // subprotocol; a session needs none of that, since the browser attaches
  // the cookie itself.
  const protocols = token ? ["rota", "bearer." + token] : ["rota"];
  let sock;
  try { sock = new WebSocket(url, protocols); }
  catch (e) { termSay("no socket: " + e); return; }
  termSock = sock;
  sock.binaryType = "arraybuffer";
  if (termStatus !== "reconnecting") { termStatus = "connecting"; renderTermStrip(); }
  sock.onopen = () => { termNote = ""; renderTermStrip(); };
  sock.onerror = () => {};
  sock.onmessage = e => {
    if (termSock !== sock) return;
    // A string is a document; anything else is what the terminal printed.
    if (typeof e.data === "string") {
      let doc;
      try { doc = JSON.parse(e.data); } catch { return; }
      termFrame(doc);
      return;
    }
    const bytes = new Uint8Array(e.data);
    // A held session's screen stands for the offset it was announced with,
    // and is not output: only what follows it moves the offset on.
    if (termPaint) termPaint = false;
    else termOffset += bytes.byteLength;
    if (termView) termView.write(bytes);
  };
  sock.onclose = ev => {
    if (termSock !== sock) return;   // an older socket saying goodbye
    termSock = null;
    if (termEnded !== null) { termStatus = "ended"; renderTermStrip(); return; }
    // 1013 is "you fell behind": the bytes are still there, so go back at
    // once for the ones this page has not been shown.
    if (ev && ev.code === 1013) { termConnect(); return; }
    termRetry();
  };
}

function termRetry() {
  if (!termId || termEnded !== null || !schema) return;
  termWait = TERM_BACKOFF[Math.min(termTries, TERM_BACKOFF.length - 1)];
  termTries++;
  termStatus = "reconnecting";
  renderTermStrip();
  const mine = termGen;
  termLater(() => { if (mine === termGen) termConnect(); }, termWait * 1000);
}

/* ============================================================= the frames == */
function termFrame(doc) {
  switch (doc && doc.type) {
    case "hello":
      termSeen = true; termTries = 0; termWait = 0;
      termOffset = doc.offset || 0;
      termPaint = doc.repaint === true;
      termKind = doc.kind || null; termMode = doc.mode || null;
      termCols = doc.cols || termCols; termRows = doc.rows || termRows;
      termHolder = doc.holder || null;
      termYou = doc.you || null;
      termViewers = doc.viewers || [];
      // Only a held session's hello says which connection holds it.
      termHeld ||= doc.holder_conn !== undefined;
      termHolderConn = doc.holder_conn || null;
      termStatus = "live";
      termFit();
      renderTerm();
      break;
    case "gap":
      // Output that was not kept is said, never papered over: a terminal
      // that looked continuous when it was not is worse than a gap.
      // Two kinds of hole, said the same way: output the scrollback no
      // longer has, and output a sharer could not get to this server at all.
      // The second carries its own count, because no offset moved for it.
      termMark("… " + (doc.bytes || ((doc.to || 0) - (doc.from || 0))) + " bytes were not kept …");
      // The offsets go on from its end.
      if (typeof doc.to === "number" && doc.to > termOffset) termOffset = doc.to;
      break;
    case "repaint":
      // A held session's viewer that fell behind: what it missed is not
      // sent, and the next binary frame is the screen as it is now, standing
      // for this offset, at this size.
      termOffset = doc.offset || 0;
      termPaint = true;
      termCols = doc.cols || termCols; termRows = doc.rows || termRows;
      termSized();
      renderTermStrip();
      break;
    case "keyboard":
      termHolder = doc.holder || null;
      if (doc.holder_conn !== undefined) termHolderConn = doc.holder_conn || null;
      if (termIHold()) termClaimAt = null; else termAsking = null;
      termFit();
      renderTerm();
      refreshTerminals();
      break;
    case "claim_request":
      // Only the holder is sent one of these, so this page is the holder.
      termAsking = { by: doc.by, id: doc.id };
      renderTermStrip();
      break;
    case "resized":
      termCols = doc.cols || termCols; termRows = doc.rows || termRows;
      // Somebody else's size is shown as it is: this page scrolls inside its
      // container rather than telling a comfortable lie about how wide the
      // terminal on the server is.
      termSized();
      renderTermStrip(); renderTermInfo();
      break;
    case "viewers":
      termViewers = doc.list || [];
      renderTermInfo();
      refreshTerminals();
      break;
    case "exit":
      termEnded = typeof doc.code === "number" ? doc.code : -1;
      termStatus = "ended";
      termHolder = null; termClaimAt = null; termAsking = null;
      if (termInfo) { termInfo.ended = true; termInfo.exit = termEnded; }
      renderTerm();
      refreshTerminals();
      break;
    case "error":
      termSay(doc.message || "that was refused");
      break;
    case "pong":
      break;
  }
}

function termSay(msg) { termNote = msg; renderTermStrip(); }

// termMark writes a line of this page's own into the terminal, dimmed so it
// cannot be mistaken for something the CLI printed.
function termMark(text) {
  if (!termView) return;
  termView.write(new TextEncoder().encode("\r\n\x1b[2m" + text + "\x1b[0m\r\n"));
}

/* =========================================================== the keyboard == */
function termTalk(frame) {
  if (!termSock) return termSay("nothing is attached");
  try { termSock.send(JSON.stringify(frame)); } catch { termSay("the socket has gone"); }
}

function termClaim(force) {
  termTalk(force ? { type: "claim", force: true } : { type: "claim" });
  if (!force && !termClaimAt) termClaimAt = termNow();
  renderTermStrip();
  termTick();
}

// termTick redraws while a request of this page's is outstanding, so the
// wait before Force is offered is visible rather than guessed at.
function termTick() {
  if (!termClaimAt || termIHold()) return;
  renderTermStrip();
  if (termNow() - termClaimAt < CLAIM_WAIT) termLater(termTick, 500);
}

// termFlash says no to a key this page may not send, once, on the control
// that says why — rather than sending it to be refused.
function termFlash() {
  if (termNudge) return;
  termNudge = true;
  renderTermStrip();
  termLater(() => { termNudge = false; renderTermStrip(); }, 500);
}

/* ============================================================== the strip == */
function renderTermStrip() {
  const host = $("#tstrip");
  if (!host) return;
  if (!termId) { host.replaceChildren(el("span", { class: "pill", text: "no terminal chosen" })); return; }
  const kids = [termBack(), el("span", { class: "pill" + termStatePill(), text: termStateText() })];
  // A terminal shared to be watched has no keyboard for anybody, so nobody
  // is shown one: the controls would be there only to be refused.
  if (termLookOnly()) kids.push(el("span", { class: "badge", text: "shared to be watched only" }));
  else if (termWatcher()) kids.push(el("span", { class: "badge", text: "watching" }));
  else if (termEnded === null) kids.push(termKeyboardBox());
  if (termAsking && termIHold()) {
    kids.push(el("span", { class: "kbd" }, [
      el("b", { text: termAsking.by }),
      el("span", { text: "wants the keyboard" }),
      el("button", { class: "btn tiny primary", text: "Grant",
        onclick: () => { termTalk({ type: "grant", to: termAsking.id }); termAsking = null; renderTermStrip(); } }),
      el("button", { class: "btn tiny", text: "Deny",
        onclick: () => { termTalk({ type: "deny", to: termAsking.id }); termAsking = null; renderTermStrip(); } }),
    ]));
  }
  kids.push(el("span", { class: "grow" }));
  kids.push(...termSizeParts());
  kids.push(termDockToggle());
  if (termNote) kids.push(el("span", { class: "pill bad", text: termNote }));
  // termBack and termDockToggle are null when they have nothing to show, and
  // a browser would draw a null child as the word.
  host.replaceChildren(...kids.filter(Boolean));
}

const termStateText = () => ({
  idle: "not attached",
  connecting: "connecting…",
  live: "live",
  reconnecting: "reconnecting in " + termWait + "s",
  ended: termEnded === null ? "ended" : "ended, code " + termEnded,
}[termStatus] || termStatus);
const termStatePill = () => ({ live: " ok", connecting: " busy", reconnecting: " busy", ended: " bad" }[termStatus] || "");

// The keyboard, in three states and no more: you have it, nobody has it, or
// somebody else has it and may be asked for it.
function termKeyboardBox() {
  const flash = termNudge ? " flash" : "";
  if (termIHold()) {
    return el("span", { class: "kbd" + flash }, [
      el("b", { text: "you" }), el("span", { text: "hold the keyboard" }),
      el("button", { class: "btn tiny", id: "kbd_release", text: "Release", onclick: () => termTalk({ type: "release" }) }),
    ]);
  }
  if (!termHolder) {
    return el("span", { class: "kbd" + flash }, [
      el("span", { text: "the keyboard is free" }),
      el("button", { class: "btn tiny primary", id: "kbd_take", text: "Take the keyboard", onclick: () => termClaim(false) }),
    ]);
  }
  const kids = [el("b", { text: termHolder }), el("span", { text: "holds the keyboard" })];
  if (!termClaimAt) {
    kids.push(el("button", { class: "btn tiny", id: "kbd_ask", text: "Ask for the keyboard", onclick: () => termClaim(false) }));
  } else {
    const left = Math.max(0, Math.ceil((CLAIM_WAIT - (termNow() - termClaimAt)) / 1000));
    if (left > 0) kids.push(el("span", { class: "pill busy", id: "kbd_waiting", text: "asked · " + left + "s" }));
    // Ten seconds unanswered is the server's rule as much as this page's: it
    // refuses a force before then, so offering one earlier would be a lie.
    else kids.push(el("button", { class: "btn tiny", id: "kbd_force", text: "Force", onclick: () => termClaim(true) }));
  }
  return el("span", { class: "kbd" + flash }, kids);
}

/* ================================================================ the page == */
function renderTerm() {
  renderTermList(); renderTermStrip(); renderTermInfo();
  const shell = $("#termshell");
  if (shell) shell.classList.toggle("open", !!termId);
  const host = $("#tscreen");
  if (host && !termId) {
    host.classList.add("nothing");
    host.replaceChildren(el("div", { class: "empty" }, [
      el("b", { text: "No terminal open" }),
      el("span", { text: "choose one on the left" })]));
  }
  renderTermDock();
}
