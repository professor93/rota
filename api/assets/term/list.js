"use strict";

/* The sessions this server holds: the list on the left, its order and its
 * two groups, the way back to it on a narrow screen, and the #<id> a link
 * opens.
 */

// termRouteNote is what the address said that this page could not open. It
// is the list's to show, at its top, because a link that opened nothing has
// no session for the strip to speak in.
let termRouteNote = "";
let termRouteHooked = false;

const termIsStopped = t => termStatusOf(t).key === "stopped";
const termWhen = iso => Date.parse(iso || "") || 0;

// termOrder is the order the list shows, and it is the list's own, never the
// server's: a row that moved under a finger is a mis-tap. What has not ended
// comes first, newest started first; then what has stopped, newest stopped
// first. A tie goes by id. Status never moves a row.
function termOrder(list) {
  const key = t => (termIsStopped(t) ? termWhen(t.ended_at || t.started) : termWhen(t.started));
  return list.slice().sort((a, b) => {
    const sa = termIsStopped(a), sb = termIsStopped(b);
    if (sa !== sb) return sa ? 1 : -1;
    const d = key(b) - key(a);
    if (d) return d;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

// termNeedsYou is how many sessions are waiting on their person.
function termNeedsYou() {
  return terms.filter(t => !termIsStopped(t) && termStatusOf(t).key === "needs_you").length;
}

// The stopped group folded away is a convenience of this device only.
function termStoppedHidden() {
  try { return localStorage.getItem("rota.stopped") === "hidden"; } catch { return false; }
}
function termStoppedToggle() {
  const hide = !termStoppedHidden();
  try {
    if (hide) localStorage.setItem("rota.stopped", "hidden");
    else localStorage.removeItem("rota.stopped");
  } catch {}
  renderTermList();
}

// termNewForm is New as the list shows it. While it is open it is one node,
// kept across every redraw, so a poll never clears what somebody is typing
// into it; it is built again only when it is folded, or when the accounts
// or the schema it offers have been fetched again.
let termNewKept = null;   // {node, accounts, schema}, while New is open
function termNewForm() {
  if (!termOpenNew) { termNewKept = null; return newTerminalForm(); }
  if (!termNewKept || termNewKept.accounts !== accounts || termNewKept.schema !== schema) {
    termNewKept = { node: newTerminalForm(), accounts, schema };
  }
  return termNewKept.node;
}

// termRow is one session, in four lines: what it is called and its status;
// what it is about; what it runs on; and its keyboard, or how it ended.
function termRow(t) {
  const shared = t.kind === "shared";
  const st = termStatusOf(t);
  const about = shared ? termSharedNote(t) : (t.title || termBase(t.cwd));
  let runs = "";
  if (t.account) {
    runs = `#${t.account.id} ${t.account.label || t.account.provider}`;
    const cred = termCredential(t, accounts);
    if (cred) runs += " · " + cred;
  } else if (!shared) runs = "a plain shell";
  let state;
  if (st.key === "stopped") {
    state = termEnding(t) + (t.ended_at ? " · " + termAgo(t.ended_at) : "");
  } else {
    state = [t.holder ? `${t.holder} has the keyboard`
                      : (shared && t.mode === "watch" ? "no keyboard" : "keyboard free"),
             `${(t.viewers || []).length} watching`].join(" · ");
  }
  const line = (cls, text) => (text ? el("span", { class: "line " + cls, text }) : null);
  return el("button", {
    class: "trow" + (st.key === "stopped" ? " over" : ""), "data-id": t.id,
    "aria-current": t.id === termId ? "true" : null,
    onclick: () => termSelect(t.id),
  }, [
    el("span", { class: "line head" }, [
      el("b", { text: termName(t) }),
      el("span", { class: "st " + st.tone, text: st.word }),
    ]),
    line("about", about),
    line("sub", runs),
    line("sub", state),
  ]);
}

function renderTermList() {
  const host = $("#tlist");
  if (!host) return;
  const need = termNeedsYou();
  try { document.title = (need ? `(${need}) ` : "") + "rota terminal"; } catch {}
  const kids = [];
  if (termRouteNote) kids.push(el("p", { class: "help warn", text: termRouteNote }));
  if (watching()) kids.push(watchNote());
  else kids.push(termNewForm());
  if (!terms.length) {
    kids.push(el("div", { class: "empty" }, [
      el("b", { text: "No terminals" }),
      el("span", { text: watching() ? "nothing is running on this server" : "start one above" }),
    ]));
    host.replaceChildren(...kids);
    return;
  }
  const live = terms.filter(t => !termIsStopped(t));
  const stopped = terms.filter(termIsStopped);
  kids.push(el("div", { class: "tgroup",
    text: `Live · ${live.length}` + (need ? ` · ${need} ${need === 1 ? "needs" : "need"} you` : "") }));
  kids.push(...live.map(termRow));
  if (stopped.length) {
    // A stopped session that is open is never folded out of sight.
    const shown = !termStoppedHidden() || stopped.some(t => t.id === termId);
    kids.push(el("button", { class: "tgroup toggle", id: "term_stopped",
      "aria-expanded": shown ? "true" : "false", text: `Stopped · ${stopped.length}`,
      onclick: termStoppedToggle }));
    if (shown) kids.push(...stopped.map(termRow));
  }
  host.replaceChildren(...kids);
}

// termBack is the way back to the list from an open session, on a screen
// too narrow for both. With nothing open there is nothing to go back from.
function termBack() {
  if (!termId) return null;
  return el("button", { class: "btn tiny back", id: "term_back", text: "← Sessions",
    onclick: () => termSelect(null) });
}

// termRoute keeps the open session in the address, so the address is a link
// to it. Replacing rather than pushing keeps the back button the browser's.
function termRoute(id) {
  try { history.replaceState(null, "", id ? "#" + id : location.pathname); } catch {}
  if (termRouteNote) { termRouteNote = ""; renderTermList(); }
}

// termRouteBoot opens the session the address names, and listens for the
// address changing. A hash counts only when it could be an id — sixteen
// lowercase hex characters — or is one this server lists; anything else is
// said, never fetched and never shown.
function termRouteBoot() {
  if (!termRouteHooked && window && window.addEventListener) {
    termRouteHooked = true;
    window.addEventListener("hashchange", () => { termRouteGo(); });
  }
  termRouteGo();
}

const termHash = () => {
  let h = "";
  try { h = String(location.hash || "").replace(/^#/, ""); } catch {}
  try { h = decodeURIComponent(h); } catch {}
  return h;
};

async function termRouteGo() {
  const h = termHash();
  if (!h) return;
  const listed = () => terms.some(t => t.id === h);
  if (/^[0-9a-f]{16}$/.test(h) || listed()) { termSelect(h); return; }
  // The listing may not have arrived yet, as on a sign-in: ask once more
  // before saying the link names nothing.
  try { await refreshTerminals(); } catch {}
  if (termHash() !== h) return;
  if (listed()) { termSelect(h); return; }
  termRouteNote = "that link names no session here";
  renderTermList();
}
