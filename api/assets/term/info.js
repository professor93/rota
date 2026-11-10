"use strict";

/* The panel on the right: what is true about the open terminal. */

function renderTermInfo() {
  const host = $("#tinfo");
  if (!host) return;
  if (!termId) {
    host.replaceChildren(el("div", { class: "empty" }, [
      el("b", { text: "Nothing open" }), el("span", { text: "choose a terminal on the left" })]));
    return;
  }
  const d = termInfo || {};
  const row = (label, value, cls) => el("div", { class: "irow" }, [
    el("dt", { text: label }), el("dd", { class: cls || null, text: value })]);
  const kids = [el("dl", {}, [
    row("Account", d.account ? `#${d.account.id} ${d.account.label || ""}`.trim() : "a plain shell"),
    d.account ? row("Provider", d.account.provider || "—") : null,
    row("Label", d.label || "—"),
    row("Folder", d.cwd || "—"),
    row("Started", termAgo(d.started)),
    row("Size", termCols + "×" + termRows),
    row("Keyboard", termEnded !== null ? "nobody"
      : (d.kind === "shared" && d.mode === "watch" ? "shared to be watched only" : (termHolder || "free"))),
    d.kind === "shared" ? row("Shared", termSharedNote(d)) : null,
    d.kind === "shared" && d.pid ? row("Its process", String(d.pid)) : null,
    row("Recording", d.recording ? "on" : "off"),
    termLapse(d.token_until, row),
    termEnded !== null
      ? row("Ended", d.lost ? (d.ending || "the sharing process went away") : "exited, code " + termEnded)
      : null,
  ])];
  kids.push(el("h4", { text: "Watching" }));
  if (!termViewers.length) kids.push(el("p", { class: "help", text: "nobody else" }));
  for (const v of termViewers) {
    kids.push(el("div", { class: "viewer" }, [
      el("span", { text: v.name }), el("span", { class: "role", text: v.role })]));
  }
  if (!watching() && termEnded === null) {
    kids.push(el("div", { style: "margin-top:14px" }, [
      el("button", { class: "btn", id: "term_kill", text: "Kill",
        title: "a hangup, and then the signal nothing survives",
        onclick: async () => {
          if (!confirm("End this terminal? Whatever is running in it is sent a hangup.")) return;
          const r = await api("DELETE", "/v1/terminals/" + encodeURIComponent(termId));
          if (!r.ok) termSay((r.doc && r.doc.error) || "that terminal could not be ended");
          refreshTerminals();
        } }),
    ]));
  }
  host.replaceChildren(...kids);
}

// termLapse is the one thing about a terminal nobody can see by looking at
// it: the login inside it has a date, and past that date the CLI in there
// stops working whatever it is in the middle of.
function termLapse(until, row) {
  if (!until) return null;
  const at = Date.parse(until);
  if (!at) return null;
  const left = at - termNow();
  if (left <= 0) return row("The login inside", "has lapsed", "warn");
  return row("The login inside", "lapses in " + termSpan(left), left < 3600000 ? "warn" : null);
}
