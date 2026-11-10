"use strict";

/* The terminals this server holds: the list on the left, its order, and,
 * once they are more than a stub, the way back to it on a narrow screen and
 * the #<id> a link opens.
 */

// termOrder is the order the list shows. Live first, then ended: what is
// still running is what anybody came for.
function termOrder(list) {
  return list.slice().sort((a, b) => (a.ended ? 1 : 0) - (b.ended ? 1 : 0));
}

function renderTermList() {
  const host = $("#tlist");
  if (!host) return;
  const kids = [];
  if (watching()) kids.push(watchNote());
  else kids.push(newTerminalForm());
  if (!terms.length) {
    kids.push(el("div", { class: "empty" }, [
      el("b", { text: "No terminals" }),
      el("span", { text: watching() ? "nothing is running on this server" : "start one above" }),
    ]));
  }
  for (const t of terms) {
    const shared = t.kind === "shared";
    const who = t.account
      ? `#${t.account.id} ${t.account.label || t.account.provider}`
      : (shared ? "" : "a plain shell");
    const acct = shared ? [who, termSharedNote(t)].filter(Boolean).join(" · ") : who;
    const sub = t.ended
      // A shared terminal has a second way of ending, and it has no code:
      // the rota holding it stopped answering.
      ? (t.lost ? (t.ending || "the sharing process went away") : `ended, code ${t.exit}`)
      : [t.holder ? `${t.holder} has the keyboard`
                  : (shared && t.mode === "watch" ? "no keyboard" : "keyboard free"),
         `${(t.viewers || []).length} watching`].join(" · ");
    kids.push(el("button", {
      class: "trow" + (t.ended ? " over" : ""), "data-id": t.id,
      "aria-current": t.id === termId ? "true" : null,
      onclick: () => termSelect(t.id),
    }, [
      el("b", { text: t.label || t.id }),
      el("span", { class: "sub", text: acct }),
      el("br"),
      el("span", { class: "sub", text: sub }),
    ]));
  }
  host.replaceChildren(...kids);
}

// termBack is the way back to the list from an open session, on a screen
// too narrow for both. Nothing yet.
function termBack() { return null; }

// termRoute keeps the open session in the address. Nothing yet.
function termRoute(id) {}

// termRouteBoot opens the session the address names. Nothing yet.
function termRouteBoot() {}
