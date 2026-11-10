"use strict";

/* New terminal: the form that starts one. */

// splitArgs splits a line the way a shell would, as far as quoting goes:
// whitespace separates, and either quote keeps a run together. It is not a
// shell — nothing here expands anything — and the server hands what comes
// out of it to the CLI verbatim.
function splitArgs(line) {
  const out = [];
  let cur = "", quote = "", started = false;
  for (const ch of String(line || "")) {
    if (quote) { if (ch === quote) quote = ""; else cur += ch; continue; }
    if (ch === '"' || ch === "'") { quote = ch; started = true; continue; }
    if (/\s/.test(ch)) { if (cur || started) { out.push(cur); cur = ""; started = false; } continue; }
    cur += ch;
  }
  if (cur || started) out.push(cur);
  return out;
}

// termAccountName is how New lists an account: by the name a person gave
// it, its e-mail, its uuid or, with none of those, its provider; and the
// provider beside it, unless that is what was shown already.
function termAccountName(a) {
  const shown = a.name || a.email || a.uuid || a.provider;
  return `#${a.id} ${shown}` + (shown === a.provider ? "" : ` · ${a.provider}`);
}

// termAsksWorktree says whether arguments already ask Claude Code for a
// worktree, in any of its spellings, so New does not ask twice.
function termAsksWorktree(list) {
  return list.some(a => a === "-w" || a === "--worktree" || a.startsWith("--worktree="));
}

const TERM_WORKTREE_ON = "Claude Code makes a git worktree for the session; files git ignores, such as .env, are not in it.";
const TERM_WORKTREE_OFF = "a worktree is Claude Code's: choose a claude account";

// newTerminalForm is what starts one: which account pays, what to call it,
// and the things a terminal is besides: arguments, a folder, a worktree,
// and whether it is an account's CLI at all. These are the TUI's New
// fields. It sends no size, so a session starts at the server's own until
// somebody fits it.
function newTerminalForm() {
  const acct = el("select", { class: "ctl", id: "nt_account" });
  acct.append(el("option", { value: "", text: "rotation's choice" }));
  for (const a of accounts) acct.append(el("option", { value: String(a.id), text: termAccountName(a) }));
  const label = el("input", { class: "ctl", id: "nt_label", placeholder: "what this one is for" });
  const args = el("input", { class: "ctl", id: "nt_args", placeholder: "--resume  \"a session id\"", spellcheck: "false" });
  const cwd = el("input", { class: "ctl", id: "nt_cwd", placeholder: "a folder to start in", spellcheck: "false" });
  const shell = el("input", { type: "checkbox", id: "nt_shell" });
  // A worktree is a Claude Code flag, so it is offered only where Claude
  // Code is sure to be what starts: a claude account chosen by id. The
  // rotation's choice might be another provider's.
  const wt = el("input", { type: "checkbox", id: "nt_worktree" });
  const wtHelp = el("span", { id: "nt_worktree_help" });
  const wtRow = el("label", { class: "toggle", for: "nt_worktree" }, [
    el("span", { class: "switch" }, [wt, el("i")]),
    el("span", { class: "t-text" }, [el("b", { text: "Worktree" }), wtHelp]),
  ]);
  const claude = () => {
    if (shell.checked || !acct.value) return false;
    const a = accounts.find(x => String(x.id) === acct.value);
    return !!a && a.provider === "claude";
  };
  // sync puts the form in step with the account and the shell. A worktree
  // that is no longer on offer is unchecked, so it never looks asked for.
  const sync = () => {
    acct.disabled = !!shell.checked;
    const on = claude();
    wt.disabled = !on;
    if (!on) wt.checked = false;
    wtHelp.textContent = on ? TERM_WORKTREE_ON : TERM_WORKTREE_OFF;
    wtRow.style.opacity = on ? "" : ".55";
  };
  acct.addEventListener("change", sync);
  shell.addEventListener("change", sync);
  sync();
  const start = el("button", { class: "btn primary", id: "nt_start", text: "Start", onclick: async () => {
    const body = {};
    if (shell.checked) body.kind = "shell";
    else if (acct.value) body.account = Number(acct.value);
    if (label.value.trim()) body.label = label.value.trim();
    const list = splitArgs(args.value);
    if (wt.checked && claude() && !termAsksWorktree(list)) list.push("--worktree");
    if (list.length) body.args = list;
    if (cwd.value.trim()) body.cwd = cwd.value.trim();
    const r = await api("POST", "/v1/terminals", body);
    if (!r.ok) { termSay((r.doc && r.doc.error) || "that terminal could not be started"); return; }
    termOpenNew = false;
    await refreshTerminals();
    if (r.doc && r.doc.id) await termSelect(r.doc.id);
  } });
  const body = el("div", { class: "body" }, [
    el("div", { class: "field" }, [el("label", { class: "flabel", for: "nt_account", text: "Account" }), acct]),
    el("div", { class: "field" }, [
      el("label", { class: "flabel", for: "nt_label", text: "Name" }),
      el("p", { class: "help", text: "how the session is listed; rota send <name> types into it" }),
      label,
    ]),
    el("div", { class: "field" }, [
      el("label", { class: "flabel", for: "nt_args", text: "Arguments" }),
      el("p", { class: "help", text: "Handed to the CLI after its own name. Quotes keep a run together." }),
      args,
    ]),
    el("div", { class: "field" }, [
      el("label", { class: "flabel", for: "nt_cwd", text: "Folder" }),
      el("p", { class: "help", text: "It starts with the environment of the rota serving this page, not your shell's." }),
      cwd,
    ]),
    wtRow,
    // Only where the server file allowed one: a terminal running an
    // account's CLI is what this is, and a general remote shell is a
    // different offer that has to be asked for.
    schema && schema.shell === true
      ? el("label", { class: "toggle", for: "nt_shell" }, [
          el("span", { class: "switch" }, [shell, el("i")]),
          el("span", { class: "t-text" }, [
            el("b", { text: "Plain shell" }),
            el("span", { text: "your login shell instead of an account's CLI" }),
          ]),
        ])
      : null,
    el("div", { style: "margin-top:9px" }, [start]),
  ]);
  const head = el("button", { class: "head", "aria-expanded": String(termOpenNew),
    onclick: () => { termOpenNew = !termOpenNew; renderTermList(); } }, [
      el("span", { text: "New terminal" }), el("span", { class: "chev", text: "›" })]);
  return el("div", { class: "group" + (termOpenNew ? " open" : ""), style: "margin-bottom:12px" }, [head, body]);
}
