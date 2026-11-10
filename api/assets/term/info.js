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
  // An id the server does not have has no description to draw, so nothing
  // of a session is made up for it: the panel says what the screen says.
  if (termMissing) {
    host.replaceChildren(el("div", { class: "empty" }, [
      el("b", { text: "No such session" }), el("span", { text: `no session ${termId} on this server` })]));
    return;
  }
  const d = termInfo || {};
  // A row's value is text, or a node that says it; either way nothing in it
  // is markup.
  const row = (label, value, cls) => el("div", { class: "irow" }, [
    el("dt", { text: label }),
    typeof value === "string"
      ? el("dd", { class: cls || null, text: value })
      : el("dd", { class: cls || null }, [value])]);
  // A held session that has stopped says so in its description, whether or
  // not this page saw it end; any other terminal has ended once its exit came.
  const ended = termEnded !== null || (!!d.held && !!d.ended);
  const st = termStatusOf(Object.assign({}, d, { ended }));
  const waiting = !ended && d.waiting_for ? ": waiting for " + termWaitingWord(d.waiting_for) : "";
  const credential = termCredential(d, accounts);
  const kids = [el("dl", {}, [
    d.held ? row("Name", termName(d)) : row("Label", d.label || "—"),
    row("Status", el("span", { class: "st " + st.tone, text: st.word + waiting })),
    d.title ? row("Title", d.title) : null,
    row("Account", d.account ? `#${d.account.id} ${d.account.label || ""}`.trim() : "a plain shell"),
    credential ? row("Credential", credential) : null,
    d.account ? row("Provider", d.account.provider || "—") : null,
    row("Folder", d.cwd || "—"),
    d.conversation_cwd && d.conversation_cwd !== d.cwd
      ? row("Worktree", termWorktree(d.conversation_cwd), "wt") : null,
    d.conversation ? row("Conversation", String(d.conversation).slice(0, 8)) : null,
    row("Started", termAgo(d.started)),
    ended && d.ended_at ? row("Stopped", termAgo(d.ended_at)) : null,
    row("Size", termCols + "×" + termRows),
    row("Keyboard", ended ? "nobody"
      : (d.kind === "shared" && d.mode === "watch" ? "shared to be watched only" : (termHolder || "free"))),
    d.kind === "shared" ? row("Shared", termSharedNote(d)) : null,
    d.kind === "shared" && d.pid ? row("Its process", String(d.pid)) : null,
    row("Recording", d.recording ? "on" : "off"),
    termLapse(d.token_until, row),
    ended ? row("Ended", d.held ? termEnding(d)
      : d.lost ? (d.ending || "the sharing process went away") : "exited, code " + termEnded) : null,
    d.made_by ? row("Made by", "rota " + d.made_by) : null,
    termLink(d, ended, row),
  ])];
  kids.push(el("h4", { text: "Watching" }));
  if (!termViewers.length) kids.push(el("p", { class: "help", text: "nobody else" }));
  for (const v of termViewers) {
    kids.push(el("div", { class: "viewer" }, [
      el("span", { text: v.name }), el("span", { class: "role", text: v.role })]));
  }
  const acts = [];
  if (!watching() && !ended) acts.push(termStopButton(d));
  // A session this page did not attach to has its actions on the screen
  // instead (core's stopped block), so no id is ever on the page twice.
  if (termStatus !== "stopped") acts.push(termActions(d));
  if (acts.some(Boolean)) kids.push(el("div", { style: "margin-top:14px" }, acts));
  host.replaceChildren(...kids);
}

// termStopButton ends the session: a held one is stopped, in its own words,
// and any other terminal is killed as it always was. Both are #term_kill.
function termStopButton(d) {
  const held = !!d.held;
  return el("button", { class: "btn", id: "term_kill", text: held ? "Stop" : "Kill",
    title: "a hangup, and then the signal nothing survives",
    onclick: async () => {
      const ask = held
        ? `Stop ${termName(d)}? Its CLI is sent a hangup. A worktree it used is left as it is.`
        : "End this terminal? Whatever is running in it is sent a hangup.";
      if (!confirm(ask)) return;
      const r = await api("DELETE", "/v1/terminals/" + encodeURIComponent(termId));
      if (!r.ok) termSay((r.doc && r.doc.error) || "that terminal could not be ended");
      refreshTerminals();
    } });
}

// termWorktree is a worktree's path with a way to copy it: it is long, and a
// person wants it in another terminal rather than read. A page served over
// plain http to another machine has no clipboard, which is said.
function termWorktree(path) {
  return el("span", {}, [
    el("span", { class: "path", text: path }),
    el("button", { class: "btn copy", id: "term_copy_wt", text: "Copy", title: "copy this path",
      onclick: async () => {
        try { await navigator.clipboard.writeText(path); }
        catch { termSay("this browser would not copy the path here"); }
      } }),
  ]);
}

// termLink is the daemon's link to a held session's holder, said only when
// it is down. A holder this rota cannot attach to is never linked to, and
// never tried again, so it is not said to be about to be.
function termLink(d, ended, row) {
  if (d.reachable !== false || ended) return null;
  if (d.made_by || d.attachable === false) return row("Link", "not reachable from this server; not tried again");
  return row("Link", "not answering; tried again shortly", "warn");
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
