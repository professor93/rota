// The panel about the open session: info.js. What it says of a held
// session, in what order, what it lets a person do to one, and that nothing
// in it is markup.
const T = process.env.T;

// Every value any element is given as markup is recorded here, before the
// kit puts its own trap around the same setter: whatever the kit lets
// through reaches this record, and only page.js's header may.
const { El } = require(T + '/dom.js');
const htmlSet = [];
{
  const own = Object.getOwnPropertyDescriptor(El.prototype, 'innerHTML');
  Object.defineProperty(El.prototype, 'innerHTML', {
    configurable: true,
    get() { return own.get.call(this); },
    set(v) { htmlSet.push(String(v)); own.set.call(this, v); },
  });
}

const kit = require(T + '/term/kit.js');
const { assert, byId, findAll } = kit;

const CLOCK = kit.CLOCK;
const ago = ms => new Date(CLOCK - ms).toISOString();
const account = { id: 1, label: 'a@x', provider: 'claude' };

// A held session at work, waiting on a permission, in a worktree of its
// folder, whose link to its holder is down for the moment.
const live = {
  id: '0123456789abcdef', kind: 'account', held: true, state: 'live', attachable: true, reachable: false,
  label: 'audit', name: 'audit-trail', title: 'Tidy the auth code', status: 'needs_you', waiting_for: 'permission',
  cwd: '/src/api', conversation_cwd: '/src/api/.claude/worktrees/w1', conversation: 'c0ffee12-3456-7890',
  started: ago(720000), cols: 120, rows: 40, holder: null, viewers: [], offset: 0, ended: false,
  recording: false, account, token_until: new Date(CLOCK + 1800000).toISOString(),
};
// The same kind of session, stopped: interrupted ten minutes ago.
const stopped = {
  id: '1123456789abcdef', kind: 'account', held: true, state: 'interrupted', attachable: false, reachable: false,
  label: '', name: 'old-work', title: '', status: 'stopped', waiting_for: '', cwd: '/src/web',
  conversation_cwd: '/src/web', conversation: 'abcdef0123', resumable: true,
  started: ago(7200000), ended_at: ago(600000), cols: 80, rows: 24, holder: null, viewers: [], offset: 0,
  ended: true, exit: 1, recording: false, account,
};
// A session another version of rota holds, which this one cannot attach to.
// Listed from its record, it is never linked to: reachable is false.
const foreign = {
  id: '2123456789abcdef', kind: 'account', held: true, state: 'live', attachable: false, reachable: false,
  made_by: '1.4.0-beta.3',
  label: '', name: 'theirs', title: '', status: 'running', waiting_for: '', cwd: '/src/x',
  started: ago(60000), cols: 80, rows: 24, holder: null, viewers: [], offset: 0, ended: false,
  recording: false, account,
};
// A held session whose title would be markup, were anything parsed.
const marked = Object.assign({}, live, { id: '3123456789abcdef', title: '<b>x</b>', reachable: true,
  conversation_cwd: '', conversation: '' });

(async () => {
  const page = await kit.load({ terminals: [live, stopped, foreign, marked] });
  const { nodes } = page;
  const tp = page.tp;
  // The credential is the account's: a long token still in date.
  tp.accounts = [{ id: 1, provider: 'claude', long_until: new Date(CLOCK + 86400000).toISOString() }];

  const panel = () => nodes['#tinfo'];
  const rows = () => findAll(panel(), e => e.tagName === 'DT').map(e => e.textContent);
  const dd = label => {
    const dt = findAll(panel(), e => e.tagName === 'DT' && e.textContent === label)[0];
    if (!dt) return null;
    const box = findAll(panel(), e => e.children && e.children.includes(dt))[0];
    return box.children.find(e => e.tagName === 'DD');
  };
  const said = label => { const d = dd(label); return d ? d.textContent : null; };
  const open = async id => { await tp.termSelect(id); await page.settle(); };

  // 1. A held live session shows every row it has, in the panel's order.
  await open(live.id);
  const want = ['Name', 'Status', 'Title', 'Account', 'Credential', 'Provider', 'Folder', 'Worktree',
    'Conversation', 'Started', 'Size', 'Keyboard', 'Recording', 'The login inside', 'Link'];
  assert(JSON.stringify(rows()) === JSON.stringify(want), 'a held live session says these rows in order: ' + JSON.stringify(rows()));
  assert(said('Name') === 'audit-trail', 'its name is its name: ' + said('Name'));
  assert(said('Status') === 'needs you: waiting for a permission', 'its status says what it waits on: ' + said('Status'));
  assert(findAll(dd('Status'), e => e.classList && e.classList.contains('st') && e.classList.contains('warn')).length === 1,
    'in the status tone of needs you');
  assert(said('Title') === 'Tidy the auth code', 'its title: ' + said('Title'));
  assert(said('Account') === '#1 a@x', 'its account: ' + said('Account'));
  assert(said('Credential') === 'long token', 'what the account launches on: ' + said('Credential'));
  assert(said('Folder') === '/src/api', 'its folder: ' + said('Folder'));
  assert(said('Worktree').startsWith('/src/api/.claude/worktrees/w1'), 'the worktree it works in: ' + said('Worktree'));
  assert(said('Conversation') === 'c0ffee12', 'the first eight of its conversation: ' + said('Conversation'));
  assert(said('Link') === 'not answering; tried again shortly' && dd('Link').classList.contains('warn'),
    'a link that is down is a warning: ' + said('Link'));
  const stop = byId(panel(), 'term_kill');
  assert(stop && stop.textContent === 'Stop', 'a held session is stopped, not killed: ' + (stop && stop.textContent));

  // Stop asks in a held session's own words, and ends it the same way.
  let asked = null;
  globalThis.confirm = q => { asked = q; return true; };
  stop.dispatch('click');
  await page.settle();
  assert(asked === 'Stop audit-trail? Its CLI is sent a hangup. A worktree it used is left as it is.',
    'stopping asks first: ' + asked);
  assert(page.fetches.some(f => f.method === 'DELETE' && f.path === '/v1/terminals/' + live.id),
    'and then stops that session');

  // 2. Copy puts the worktree's path on the clipboard.
  const copy = byId(panel(), 'term_copy_wt');
  assert(copy && copy.textContent === 'Copy', 'the worktree can be copied');
  copy.dispatch('click');
  await page.settle();
  assert(page.clipboard.length === 1 && page.clipboard[0] === live.conversation_cwd,
    'Copy writes the worktree path: ' + JSON.stringify(page.clipboard));

  // The actions follow the button, unless the screen already shows them
  // (a session the page did not attach to): no id is ever on the page twice.
  const realActions = tp.termActions;
  tp.termActions = () => tp.el('div', { id: 'probe_actions' });
  tp.renderTermInfo();
  assert(byId(panel(), 'probe_actions'), 'the actions follow the button');
  const status = tp.termStatus;
  tp.termStatus = 'stopped';
  tp.renderTermInfo();
  assert(!byId(panel(), 'probe_actions'), 'and are not in the panel while the screen shows them');
  tp.termStatus = status;
  tp.termActions = realActions;

  // 3. A stopped session has no Stop, says when it stopped, and how it ended.
  await open(stopped.id);
  tp.termEnded = null; // whatever the socket did, the description says it has stopped
  tp.renderTermInfo();
  assert(!byId(panel(), 'term_kill'), 'a stopped session cannot be stopped again');
  assert(said('Stopped') === '10m ago', 'it says when it stopped: ' + said('Stopped'));
  assert(said('Ended') === 'interrupted', 'and how it ended: ' + said('Ended'));
  assert(said('Status') === 'stopped', 'its status is stopped: ' + said('Status'));
  assert(!rows().includes('Worktree'), 'a worktree that is its folder is not said twice');
  assert(!rows().includes('Title') && !rows().includes('Link') && !rows().includes('Made by'),
    'rows it has no reason for are left out: ' + JSON.stringify(rows()));
  const r = rows();
  assert(r.indexOf('Started') < r.indexOf('Stopped') && r.indexOf('Stopped') < r.indexOf('Size'),
    'Stopped comes after Started: ' + JSON.stringify(r));

  // 4. A foreign holder says who made it, and can still be stopped.
  await open(foreign.id);
  assert(said('Made by') === 'rota 1.4.0-beta.3', 'it says which rota holds it: ' + said('Made by'));
  assert(byId(panel(), 'term_kill') && byId(panel(), 'term_kill').textContent === 'Stop', 'and still offers Stop');
  assert(said('Link') === 'not reachable from this server; not tried again',
    'a holder this rota cannot attach to is not said to be tried again: ' + said('Link'));
  assert(!dd('Link').classList.contains('warn'), 'and is not a passing warning');
  // A link nobody said is down is not said to be.
  tp.termInfo = Object.assign({}, foreign, { reachable: undefined });
  tp.renderTermInfo();
  assert(!rows().includes('Link'), 'a link nobody said is down is not said to be: ' + JSON.stringify(rows()));

  // 5. A watcher gets no Stop.
  const me = tp.me;
  tp.me = Object.assign({}, me, { role: 'watch' });
  tp.renderTermInfo();
  assert(!byId(panel(), 'term_kill'), 'a watcher cannot stop a session');
  tp.me = me;

  // 6. a title with markup stays text (Review Focus 4).
  await open(marked.id);
  const title = dd('Title');
  assert(title && title.textContent === '<b>x</b>', 'the title is said as it is: ' + (title && title.textContent));
  assert(title.innerHTML === '' && !findAll(title, e => e.tagName).length,
    'and nothing in its row was parsed into elements');
  assert(htmlSet.length > 0 && htmlSet.every(v => v === tp.HEADER),
    'the only markup the page ever set is its header: ' + JSON.stringify(htmlSet.filter(v => v !== tp.HEADER)));

  // A terminal that is not held keeps its Label and its Kill.
  page.terminals.push({ id: 't-plain', kind: 'shell', label: 'mine', cwd: '/', started: ago(1000),
    cols: 80, rows: 24, holder: null, viewers: [], offset: 0, ended: false, recording: false });
  await open('t-plain');
  assert(rows()[0] === 'Label' && !rows().includes('Name'), 'a terminal that is not held keeps Label: ' + JSON.stringify(rows()));
  assert(byId(panel(), 'term_kill').textContent === 'Kill', 'and Kill');
  assert(!rows().includes('Credential'), 'a shell launches on no credential');

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
