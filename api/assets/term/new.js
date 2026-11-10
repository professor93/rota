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

// newTerminalForm is what starts one: which account pays, what to call it,
// and the three things a terminal is besides — arguments, a folder, and
// whether it is an account's CLI at all.
function newTerminalForm() {
  const acct = el("select", { class: "ctl", id: "nt_account" });
  acct.append(el("option", { value: "", text: "rotation's choice" }));
  for (const a of accounts) {
    acct.append(el("option", { value: String(a.id), text: `#${a.id} ${a.email || a.uuid || a.provider}` }));
  }
  const label = el("input", { class: "ctl", id: "nt_label", placeholder: "what this one is for" });
  const args = el("input", { class: "ctl", id: "nt_args", placeholder: "--resume  \"a session id\"", spellcheck: "false" });
  const cwd = el("input", { class: "ctl", id: "nt_cwd", placeholder: "a folder to start in", spellcheck: "false" });
  const shell = el("input", { type: "checkbox", id: "nt_shell" });
  shell.addEventListener("change", () => { acct.disabled = !!shell.checked; });
  const start = el("button", { class: "btn primary", id: "nt_start", text: "Start", onclick: async () => {
    const body = {};
    if (shell.checked) body.kind = "shell";
    else if (acct.value) body.account = Number(acct.value);
    if (label.value.trim()) body.label = label.value.trim();
    const list = splitArgs(args.value);
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
    el("div", { class: "field" }, [el("label", { class: "flabel", for: "nt_label", text: "Label" }), label]),
    el("div", { class: "field" }, [
      el("label", { class: "flabel", for: "nt_args", text: "Arguments" }),
      el("p", { class: "help", text: "Handed to the CLI after its own name. Quotes keep a run together." }),
      args,
    ]),
    el("div", { class: "field" }, [el("label", { class: "flabel", for: "nt_cwd", text: "Folder" }), cwd]),
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
