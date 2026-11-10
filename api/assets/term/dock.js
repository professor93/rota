"use strict";

/* The dock under the screen: a composer that types a whole message into a
 * held session once it is ready, and the keys a CLI asks for, for a phone
 * whose own keyboard has neither an Esc nor an arrow.
 *
 * What it types goes into a live CLI, so it is careful in both directions:
 * a message is sent once, only when a person presses Send, and never again
 * by itself; and a key is exactly its bytes, as one frame, only under a
 * keyboard this page holds or may claim.
 */

const TERM_DOCK_KEY = "rota.dock";
const TERM_SEND_MAX = 256 << 10;   // the server's bound on a send's body
const TERM_COARSE = "(hover:none) and (pointer:coarse)";

// The quick keys, in the order they are shown. An arrow has two spellings,
// and which one a CLI reads is its cursor-key mode, so both are here.
const TERM_KEYS = [
  { key: "1", label: "1", bytes: "1" },
  { key: "2", label: "2", bytes: "2" },
  { key: "3", label: "3", bytes: "3" },
  { key: "esc", label: "Esc", bytes: "\x1b" },
  { key: "enter", label: "⏎", aria: "Enter", bytes: "\r" },
  { key: "tab", label: "Tab", bytes: "\t" },
  { key: "up", label: "↑", aria: "Up", bytes: "\x1b[A", app: "\x1bOA" },
  { key: "down", label: "↓", aria: "Down", bytes: "\x1b[B", app: "\x1bOB" },
  { key: "left", label: "←", aria: "Left", bytes: "\x1b[D", app: "\x1bOD" },
  { key: "right", label: "→", aria: "Right", bytes: "\x1b[C", app: "\x1bOC" },
  { key: "ctrl-c", label: "^C", aria: "Ctrl-C", bytes: "\x03" },
];

// termDrafts is each session's text while another one is open. It lives in
// memory only: no message is ever stored.
const termDrafts = new Map();
const termDockOut = new Set();      // the sessions with a send out
const termDockNotes = new Map();    // id → {text, force}: what the last send came to
let termDockPick = null;            // this page's own toggle, if storage would not keep it
let termDockFor = null;             // the session the box holds the text of
let termDockWho = null;             // whose drafts these are
let termDockParts = null;           // the dock's nodes, made once

/* ============================================================ showing it == */
// termDockShown is whether this device wants the dock: what it said, else
// whether its pointer is a finger.
function termDockShown() {
  let said = termDockPick;
  if (said === null) {
    try { said = localStorage.getItem(TERM_DOCK_KEY); } catch {}
  }
  if (said === "on") return true;
  if (said === "off") return false;
  try { return !!(window.matchMedia && window.matchMedia(TERM_COARSE).matches); } catch { return false; }
}

// termDockToggle is the strip's way to show or hide the dock, offered to
// whoever may type into the open session.
function termDockToggle() {
  if (!termId || !termCanType()) return null;
  const shown = termDockShown();
  return el("button", { class: "btn tiny", id: "dock_toggle", type: "button", text: "Composer",
    "aria-pressed": shown ? "true" : "false",
    onclick: () => {
      termDockPick = shown ? "off" : "on";
      try { localStorage.setItem(TERM_DOCK_KEY, termDockPick); } catch {}
      renderTermStrip();
      renderTermDock();
    } });
}

/* ================================================================ the keys == */
// termKeyBytes is what a quick key types, or "" for a name that is not one.
function termKeyBytes(name, appCursor) {
  const k = TERM_KEYS.find(x => x.key === name);
  if (!k) return "";
  return appCursor && k.app ? k.app : k.bytes;
}

function termAppCursor() {
  try { return !!(termView && typeof termView.appCursor === "function" && termView.appCursor()); }
  catch { return false; }
}

// termSendKey types one quick key: as one binary frame, so the server sees
// an Esc or a Ctrl-C alone as the interrupt it is and an arrow never as an
// Esc; under this page's keyboard, or a free one claimed first, and never
// under another person's.
function termSendKey(name) {
  const bytes = termKeyBytes(name, termAppCursor());
  if (!bytes) return;
  if (!termCanType()) return termFlash();
  if (!termSock) return termSay("nothing is attached");
  if (!termIHold()) {
    if (termHolder) { termFlash(); termSay(`${termHolder} holds the keyboard`); return; }
    termTalk({ type: "claim" });
  }
  try { termSock.send(new TextEncoder().encode(bytes)); } catch { termSay("the socket has gone"); }
}

/* ============================================================ the composer == */
// termCompose sends the open session's text, once. Nothing here ever sends it
// again: a send whose answer was lost may have been typed, and only the
// person looking at the screen can tell.
async function termCompose(force) {
  const id = termId;
  const p = termDockParts;
  if (!id || !p || termDockFor !== id || !termHeld || !termCanType() || termDockOut.has(id)) return;
  const raw = String(p.text.value);
  termDrafts.set(id, raw);
  const text = raw.replace(/\s+$/u, "");
  if (!text) return;
  const body = force ? { text, force: true } : { text };
  if (new TextEncoder().encode(JSON.stringify(body)).length > TERM_SEND_MAX) {
    termDockNotes.set(id, { text: "a message is at most 256 KiB" });
    return renderTermDock();
  }
  termDockOut.add(id);
  termDockNotes.delete(id);
  renderTermDock();
  let r = null;
  try { r = await api("POST", "/v1/terminals/" + encodeURIComponent(id) + "/messages", body); } catch {}
  termDockOut.delete(id);
  if (!r) {
    termDockNotes.set(id, { text: "the message may not have been sent; look at the screen before sending it again" });
  } else if (r.ok) {
    termDockSent(id, raw);
  } else {
    const doc = r.doc && typeof r.doc === "object" ? r.doc : {};
    termDockNotes.set(id, { text: String(doc.error || "the server answered " + r.status),
      force: r.status === 409 && doc.reason === "waiting" });
  }
  renderTermDock();
}

// termDockSent clears what was typed in. Text added after the send went out
// is kept, and an edit made meanwhile is kept whole: a 200 is about what
// was sent, not about what the box holds now.
function termDockSent(id, raw) {
  const now = termDockFor === id && termDockParts ? String(termDockParts.text.value) : (termDrafts.get(id) || "");
  const left = now === raw ? "" : now.startsWith(raw) ? now.slice(raw.length) : now;
  if (left) termDrafts.set(id, left); else termDrafts.delete(id);
  if (termDockFor === id && termDockParts) termDockParts.text.value = left;
}

/* ================================================================ drawing == */
// termDockBuild makes the dock's nodes once. A phone's keyboard stays up
// only while its box is the same box, so a redraw moves these and never
// makes them again.
function termDockBuild() {
  if (termDockParts) return termDockParts;
  const p = {};
  p.text = el("textarea", { id: "dock_text", class: "ctl", rows: "2", autocapitalize: "sentences",
    spellcheck: "true", "aria-label": "Message",
    placeholder: "Write a message; it is typed in once the session is ready",
    oninput: () => { if (termDockFor) termDrafts.set(termDockFor, String(p.text.value)); termDockButtons(); },
    onkeydown: e => {
      // Enter is a new line. Ctrl+Enter or ⌘+Enter sends, but not while an
      // input method is still composing what that Enter would commit.
      if (e.key !== "Enter" || !(e.ctrlKey || e.metaKey) || e.isComposing || e.keyCode === 229) return;
      e.preventDefault();
      termCompose(false);
    } });
  p.send = el("button", { class: "btn primary", id: "dock_send", type: "button", text: "Send",
    onclick: () => termCompose(false) });
  p.force = el("button", { class: "btn", id: "dock_force", type: "button", text: "Send anyway",
    onclick: () => termCompose(true) });
  p.say = el("p", { class: "help docksay", id: "dock_say", role: "status", "aria-live": "polite" });
  p.row = el("div", { class: "dockrow" });
  p.plain = el("p", { class: "help", text: "the composer types into held sessions; this terminal takes keys directly" });
  p.compose = el("div", { class: "dockcompose", id: "dock_compose" });
  p.ask = el("p", { class: "dockask", id: "dock_ask" });
  p.keys = el("div", { class: "dockkeys", id: "dock_keys", role: "group", "aria-label": "Keys" },
    TERM_KEYS.map(k => el("button", { class: "btn dockkey", type: "button", "data-key": k.key,
      "aria-label": k.aria, text: k.label,
      // A key pressed is not a reason to take the focus from the box, which
      // would put a phone's keyboard away.
      onmousedown: e => e.preventDefault(),
      onclick: () => termSendKey(k.key) })));
  termDockParts = p;
  return p;
}

// termDockPut gives a node these children, unless it has them already in
// this order, so that a box being typed into is not moved for nothing.
function termDockPut(host, kids) {
  const now = Array.from(host.children || []);
  if (now.length === kids.length && kids.every((k, i) => now[i] === k)) return;
  host.replaceChildren(...kids);
}

// termDockButtons is Send and its note, as the open session's send stands.
function termDockButtons() {
  const p = termDockParts;
  if (!p || !termDockFor) return;
  const out = termDockOut.has(termDockFor);
  const note = termDockNotes.get(termDockFor);
  p.send.textContent = out ? "Sending…" : "Send";
  p.send.disabled = out || !String(p.text.value).trim();
  p.force.disabled = out;
  p.say.textContent = note ? note.text : "";
  termDockPut(p.row, note && note.force ? [p.say, p.force, p.send] : [p.say, p.send]);
}

function renderTermDock() {
  const host = $("#tdock");
  if (!host) return;
  const p = termDockBuild();
  // Drafts are one person's: another signing in on this tab starts empty.
  const who = me ? me.name : null;
  if (who !== termDockWho) { termDrafts.clear(); termDockNotes.clear(); termDockWho = who; termDockFor = null; }
  // The box follows the open session: what it held is that session's draft.
  if (termDockFor !== termId) {
    if (termDockFor) {
      const v = String(p.text.value);
      if (v) termDrafts.set(termDockFor, v); else termDrafts.delete(termDockFor);
    }
    termDockFor = termId;
    p.text.value = (termId && termDrafts.get(termId)) || "";
  }
  const shown = !!termId && termCanType() && termDockShown();
  hide(host, !shown);
  host.classList.toggle("asks", shown && termWaiting());
  if (!shown) return;
  if (termHeld) {
    termDockPut(p.compose, [p.text, p.row]);
    termDockButtons();
  } else {
    termDockPut(p.compose, [p.plain]);
  }
  if (termWaiting()) {
    const what = termWaitingWord((termState && termState.waiting_for) || "") || "you";
    p.ask.textContent = `${termName(termInfo || { id: termId })} is waiting for ${what}: answer with these keys`;
    termDockPut(host, [p.ask, p.keys, p.compose]);
  } else {
    termDockPut(host, [p.compose, p.keys]);
  }
}
