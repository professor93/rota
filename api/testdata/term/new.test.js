// New: the fields the TUI's New has. A name, the account or the rotation's
// choice, arguments, a folder, and a worktree, which is Claude Code's own and
// so is offered only for a claude account chosen by id. The fixture's
// accounts are 1 claude, 2 codex, 3 claude and 4 claude (dead).
const kit = require(process.env.T + '/term/kit.js');
const { assert, byId } = kit;

(async () => {
  const page = await kit.load({ signedIn: true, role: 'control' });
  const { nodes, tp } = page;
  const posted = [];
  page.route('POST', '/v1/terminals', (path, body) => { if (path === '/v1/terminals') posted.push(body || {}); });

  // form opens New afresh and hands back its fields.
  const form = () => {
    tp.termOpenNew = true; tp.renderTerm();
    const f = {};
    for (const k of ['account', 'label', 'args', 'cwd', 'shell', 'worktree', 'start']) f[k] = byId(nodes['#tlist'], 'nt_' + k);
    f.wtHelp = byId(nodes['#tlist'], 'nt_worktree_help');
    return f;
  };
  const choose = (f, id) => { f.account.value = id; f.account.dispatch('change'); };
  const start = async f => {
    const n = posted.length;
    f.start.dispatch('click', { target: f.start });
    await page.settle(); await page.settle();
    assert(posted.length === n + 1, 'Start posts one terminal');
    return posted[posted.length - 1];
  };
  const ON = "Claude Code makes a git worktree for the session; files git ignores, such as .env, are not in it.";
  const OFF = "a worktree is Claude Code's: choose a claude account";

  // The label is the session's name, and says what the name is for.
  let f = form();
  assert(f.label && f.worktree && f.wtHelp && f.shell, 'New has a name, a worktree and a plain shell');
  const text = () => nodes['#tlist'].textContent;
  const labels = byId(nodes['#tlist'], 'nt_label') && kit.findAll(nodes['#tlist'], e => e.tagName === 'LABEL' && e.attrs.for === 'nt_label');
  assert(labels.length === 1 && labels[0].textContent === 'Name', 'the label field reads Name: ' + (labels[0] && labels[0].textContent));
  assert(text().includes('how the session is listed; rota send <name> types into it'), 'and says what a name is for');
  assert(text().includes("It starts with the environment of the rota serving this page, not your shell's."),
    'the folder says whose environment a session starts with');

  // Each account says its provider beside a name that is not the provider.
  const opts = f.account.children.map(o => o.textContent);
  assert(opts[0] === "rotation's choice" && opts[1] === '#1 a@x · claude' && opts[2] === '#2 c@x · codex' && opts[3] === '#3 b@x · claude',
    'the accounts are listed with their providers: ' + JSON.stringify(opts));

  // The rotation's choice might be a codex account, so no worktree.
  assert(f.worktree.disabled && f.wtHelp.textContent === OFF, "the rotation's choice offers no worktree: " + f.wtHelp.textContent);
  choose(f, '2');
  assert(f.worktree.disabled && f.wtHelp.textContent === OFF, 'nor does a codex account');
  choose(f, '1');
  assert(!f.worktree.disabled && f.wtHelp.textContent === ON, 'a claude account does: ' + f.wtHelp.textContent);
  choose(f, '3');
  assert(!f.worktree.disabled, 'any claude account does');

  // Checked, --worktree is the last argument, and nothing else is added.
  choose(f, '1');
  f.worktree.checked = true; f.worktree.dispatch('change');
  f.args.value = '--resume "an id"'; f.label.value = 'audit'; f.cwd.value = '/src';
  let b = await start(f);
  assert(JSON.stringify(b.args) === '["--resume","an id","--worktree"]', 'a worktree is asked for last: ' + JSON.stringify(b.args));
  assert(b.account === 1 && b.label === 'audit' && b.cwd === '/src', 'with the rest of the form: ' + JSON.stringify(b));
  assert(!('cols' in b) && !('rows' in b), 'New sends no size: ' + JSON.stringify(b));
  assert(Object.keys(b).sort().join() === 'account,args,cwd,label', 'and no field of its own beyond these: ' + JSON.stringify(b));

  // No arguments of its own beside a worktree.
  f = form(); choose(f, '1'); f.worktree.checked = true; f.worktree.dispatch('change');
  b = await start(f);
  assert(JSON.stringify(b.args) === '["--worktree"]', 'a worktree alone is one argument: ' + JSON.stringify(b.args));

  // Asked for already, it is not asked for twice.
  for (const given of ['-w', '--worktree', '--worktree=feature', '-w feature --resume x']) {
    f = form(); choose(f, '1'); f.worktree.checked = true; f.worktree.dispatch('change');
    f.args.value = given;
    b = await start(f);
    assert(JSON.stringify(b.args) === JSON.stringify(tp.splitArgs(given)), `after ${given} it is not doubled: ` + JSON.stringify(b.args));
  }

  // Unchecked, nothing is added.
  f = form(); choose(f, '1'); f.args.value = '-x';
  b = await start(f);
  assert(JSON.stringify(b.args) === '["-x"]', 'unchecked, the arguments are as typed: ' + JSON.stringify(b.args));

  // A plain shell is no claude, so it disables the worktree, and a worktree
  // checked before cannot ride along.
  f = form(); choose(f, '1'); f.worktree.checked = true; f.worktree.dispatch('change');
  f.shell.checked = true; f.shell.dispatch('change');
  assert(f.worktree.disabled && f.wtHelp.textContent === OFF, 'a plain shell disables the worktree');
  b = await start(f);
  assert(b.kind === 'shell' && !('args' in b), 'and a shell is started without one: ' + JSON.stringify(b));
  f = form(); choose(f, '1'); f.shell.checked = true; f.shell.dispatch('change');
  f.shell.checked = false; f.shell.dispatch('change');
  assert(!f.worktree.disabled, 'turning the shell off offers it again');

  // Moving from a claude account to another kind takes the worktree away.
  f = form(); choose(f, '1'); f.worktree.checked = true; f.worktree.dispatch('change');
  choose(f, '2');
  b = await start(f);
  assert(b.account === 2 && !('args' in b), 'a codex account is started without one: ' + JSON.stringify(b));
  f = form(); choose(f, '1'); f.worktree.checked = true; f.worktree.dispatch('change');
  choose(f, '');
  b = await start(f);
  assert(!('account' in b) && !('args' in b), "so is the rotation's choice: " + JSON.stringify(b));

  // No Remote Control in any spelling: over TCP the server refuses each.
  for (const p of posted) {
    assert(!('remote_control' in p), 'New sends no remote_control: ' + JSON.stringify(p));
    for (const a of p.args || []) {
      assert(!/^(--remote-control|--rc|--settings|remote-control)/.test(a), 'nor an argument asking for it: ' + JSON.stringify(p));
    }
  }

  // A name the person gave wins over the e-mail, and an account named by its
  // provider alone does not say it twice. A name is text, whatever it holds.
  tp.accounts = tp.accounts.concat([
    { id: 8, provider: 'claude', name: '<b>x</b>', email: 'n@x' },
    { id: 9, provider: 'grok' },
  ]);
  f = form();
  const more = f.account.children.map(o => o.textContent);
  assert(more.includes('#8 <b>x</b> · claude'), 'a name is shown in place of the e-mail, as text: ' + JSON.stringify(more));
  assert(more.includes('#9 grok'), 'and the provider is not said twice: ' + JSON.stringify(more));
  choose(f, '8');
  assert(!f.worktree.disabled, 'a named claude account offers a worktree');
  choose(f, '9');
  assert(f.worktree.disabled, 'a grok account does not');
  assert(page.markup.length === 0, 'nothing was written as markup: ' + JSON.stringify(page.markup));

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
