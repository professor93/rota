// The session list: its order, what a row says, its two groups, the
// /terminal#<id> a link opens, and a link or a name that holds markup.
const kit = require(process.env.T + '/term/kit.js');
const { assert, byId, findAll, btn, CLOCK } = kit;

const HEX = '0123456789abcdef';
const ago = ms => new Date(CLOCK - ms).toISOString();
const H = 3600000;

// Five sessions. Two live ones started at the same moment, so only their ids
// can order them; a stopped one without ended_at is ordered by when it
// started, which here is later than the other stopped one ended.
const five = () => [
  { id: HEX, kind: 'account', held: true, status: 'needs_you', title: 'fix the build', label: '',
    cwd: '/src/app', started: ago(10 * 60000), holder: null, viewers: [], ended: false,
    account: { id: 1, label: 'a@x', provider: 'claude' } },
  { id: 't-live2', kind: 'account', label: 'fintech', cwd: '/src/api', started: ago(10 * 60000),
    holder: 'alice', viewers: [{ name: 'alice', role: 'control' }, { name: 'looker', role: 'watch' }],
    ended: false, account: { id: 2, label: 'k@x', provider: 'claude' } },
  { id: 't-new3', kind: 'shell', label: '', cwd: '/home/me/notes', started: ago(60000),
    holder: null, viewers: [], ended: false },
  { id: 't-int', kind: 'account', held: true, status: 'stopped', state: 'interrupted', name: 'night run',
    cwd: '/src', started: ago(5 * H), ended_at: ago(2 * H), ended: true, holder: null, viewers: [],
    account: { id: 1, label: 'a@x', provider: 'claude' } },
  { id: 't-done', kind: 'shell', label: '', cwd: '/', started: ago(H), ended: true, exit: 2,
    holder: null, viewers: [] },
];
// Newest live first, a tie by id; then newest stopped first.
const WANT = ['t-new3', HEX, 't-live2', 't-done', 't-int'];

(async () => {
  const page = await kit.load({ terminals: five(), signedIn: false, hash: '#' + HEX });
  const { nodes, fetches, history } = page;
  const tp = page.tp;
  page.route('GET', '/v1/accounts', () => ({ status: 200, doc: { accounts: [
    { id: 1, provider: 'claude', email: 'a@x' },
    { id: 2, provider: 'claude', email: 'k@x', credential: 'apikey' },
  ], default: 1 } }));
  const settle = async () => { await page.settle(); await page.settle(); };
  const list = () => nodes['#tlist'];
  const rows = () => findAll(list(), e => e.classList && e.classList.contains('trow'));
  const ids = () => rows().map(r => r.attrs['data-id']);
  const row = id => rows().find(r => r.attrs['data-id'] === id);
  const text = id => (row(id) ? row(id).textContent : '(no row ' + id + ')');

  // Routing: a link naming 16 hex characters opens that session on sign-in.
  await tp.signIn('driver', 'right', () => {});
  await settle();
  assert(fetches.some(f => f.method === 'GET' && f.path === '/v1/terminals/' + HEX),
    'a /terminal#<hex> link asks for that session: ' + JSON.stringify(fetches.map(f => f.path)));
  assert(tp.termId === HEX, 'and opens it: ' + tp.termId);
  assert(page.listeners.hashchange && page.listeners.hashchange.length === 1, 'a hashchange is listened for');

  // Order: the same five, served in three shuffled orders, are the same rows.
  for (const order of [[0, 1, 2, 3, 4], [4, 3, 2, 1, 0], [3, 0, 4, 2, 1]]) {
    const all = five();
    page.terminals.splice(0, page.terminals.length, ...order.map(i => all[i]));
    await tp.refreshTerminals();
    assert(JSON.stringify(ids()) === JSON.stringify(WANT),
      'served as ' + order + ', the rows are in their own order: ' + JSON.stringify(ids()));
  }
  // A status change never moves a row.
  page.terminals.find(t => t.id === 't-live2').status = 'needs_you';
  await tp.refreshTerminals();
  assert(JSON.stringify(ids()) === JSON.stringify(WANT), 'a status that changes moves nothing: ' + JSON.stringify(ids()));
  page.terminals.find(t => t.id === 't-live2').status = undefined;
  await tp.refreshTerminals();

  // Row text.
  const need = text(HEX);
  for (const want of ['needs you', 'fix the build', '#1 a@x · login', 'keyboard free · 0 watching']) {
    assert(need.includes(want), 'a held session that needs you says ' + JSON.stringify(want) + ': ' + need);
  }
  const chip = findAll(row(HEX), e => e.classList && e.classList.contains('st'))[0];
  assert(chip && chip.classList.contains('warn') && chip.textContent === 'needs you',
    'its status is a chip in the warning tone: ' + (chip && chip.textContent));
  const named = findAll(row(HEX), e => e.tagName === 'B')[0];
  assert(named && named.textContent === HEX, 'a session with no name or label is called by its id');
  const int = text('t-int');
  assert(int.includes('night run') && int.includes('stopped') && int.includes('interrupted · 2h'),
    'an interrupted one says it stopped, how, and when: ' + int);
  assert(text('t-live2').includes('#2 k@x · API key'), 'an account on an API key says so: ' + text('t-live2'));
  assert(text('t-new3').includes('notes') && text('t-new3').includes('a plain shell'),
    'a session with no title is its folder, and a shell says so: ' + text('t-new3'));
  // What page.test.js reads off a terminal that is not held is all still there.
  const live2 = text('t-live2');
  for (const want of ['fintech', '#2 k@x', 'alice has the keyboard', '2 watching']) {
    assert(live2.includes(want), 'a terminal that is not held still says ' + JSON.stringify(want) + ': ' + live2);
  }
  assert(text('t-done').includes('ended, code 2') && text('t-done').includes('ended'), 'an ended one says how: ' + text('t-done'));
  page.terminals.push(
    { id: 't-shared', kind: 'shared', label: 'api', cwd: '/src/api', started: ago(30 * 60000), holder: null,
      viewers: [], ended: false, mode: 'control', account: { id: 1, label: 'a@x', provider: 'claude' } },
    { id: 't-look', kind: 'shared', label: 'demo', cwd: '/src/demo', started: ago(30 * 60000), holder: null,
      viewers: [], ended: false, mode: 'watch' });
  await tp.refreshTerminals();
  assert(text('t-shared').includes('shared from a local terminal') && text('t-shared').includes('#1 a@x'),
    'a shared terminal says what it is and who pays: ' + text('t-shared'));
  assert(text('t-look').includes('shared from a local terminal, to watch'),
    'and one shared to watch says so: ' + text('t-look'));
  page.terminals.splice(5);
  await tp.refreshTerminals();

  // Groups.
  const heads = () => findAll(list(), e => e.classList && e.classList.contains('tgroup'));
  assert(heads().length === 2, 'there are two group headers: ' + heads().map(h => h.textContent));
  assert(heads()[0].textContent === 'Live · 3 · 1 needs you', 'the live header counts: ' + heads()[0].textContent);
  assert(heads()[1].tagName === 'BUTTON' && heads()[1].textContent === 'Stopped · 2',
    'the stopped header is a button that counts: ' + heads()[1].tagName + ' ' + heads()[1].textContent);
  assert(document.title === '(1) rota terminal', 'the tab says how many need you: ' + document.title);
  page.terminals.find(t => t.id === 't-live2').status = 'needs_you';
  await tp.refreshTerminals();
  assert(heads()[0].textContent === 'Live · 3 · 2 need you', 'and says need for more than one: ' + heads()[0].textContent);
  assert(document.title === '(2) rota terminal', 'as does the tab: ' + document.title);
  page.terminals.find(t => t.id === 't-live2').status = undefined;
  await tp.refreshTerminals();

  // Choosing a row keeps it in the address.
  const r2 = row('t-live2');
  r2.dispatch('click', { target: r2 });
  await settle();
  assert(tp.termId === 't-live2', 'a row opens its session');
  let last = history[history.length - 1];
  assert(last && last[2] === '#t-live2', 'and the address names it: ' + JSON.stringify(last));
  assert(row('t-live2').attrs['aria-current'] === 'true', 'and the row says it is the one open');

  // The Stopped toggle hides its rows and is remembered on this device.
  const toggle = heads()[1];
  toggle.dispatch('click', { target: toggle });
  assert(!row('t-int') && !row('t-done'), 'hiding the stopped group hides its rows: ' + JSON.stringify(ids()));
  assert(page.storage.getItem('rota.stopped') === 'hidden', 'and is remembered: ' + page.storage.getItem('rota.stopped'));
  assert(heads()[1].attrs['aria-expanded'] === 'false', 'and the header says it is folded');
  await tp.refreshTerminals();
  assert(!row('t-int'), 'it stays hidden across a poll');
  // A stopped session that is open always shows its group.
  await tp.termSelect('t-int');
  await settle();
  assert(row('t-int') && row('t-done'), 'an open stopped session shows its group: ' + JSON.stringify(ids()));
  await tp.termSelect('t-live2');
  await settle();
  assert(!row('t-int'), 'and once it is not open, the group folds again');
  heads()[1].dispatch('click', { target: heads()[1] });
  assert(row('t-int') && page.storage.getItem('rota.stopped') === null, 'unfolding it is remembered too');

  // ← Sessions goes back to the list, and the address forgets the session.
  const back = btn(nodes['#tstrip'], '← Sessions');
  assert(back && back.attrs.id === 'term_back' && back.classList.contains('back'), 'an open session has a way back');
  back.dispatch('click', { target: back });
  await settle();
  assert(tp.termId === null, 'which closes it');
  last = history[history.length - 1];
  assert(last && last[2] === '/terminal', 'and takes the hash away: ' + JSON.stringify(last));
  assert(!btn(nodes['#tstrip'], '← Sessions') && tp.termBack() === null, 'with nothing open there is nothing to go back from');

  // A hashchange to a listed id opens it.
  location.hash = '#t-new3';
  page.listeners.hashchange.forEach(f => f());
  await settle();
  assert(tp.termId === 't-new3', 'a hashchange to a listed id opens it: ' + tp.termId);
  await tp.termSelect(null);
  await settle();

  // An open New form keeps what was typed into it across a poll.
  tp.termOpenNew = true; tp.renderTerm();
  const typedLabel = byId(list(), 'nt_label');
  const typedArgs = byId(list(), 'nt_args');
  assert(typedLabel && typedArgs, 'New is open');
  typedLabel.value = 'keep me'; typedArgs.value = '--resume x';
  await tp.refreshTerminals();
  tp.renderTerm();
  assert(byId(list(), 'nt_label') === typedLabel && byId(list(), 'nt_label').value === 'keep me' &&
    byId(list(), 'nt_args').value === '--resume x', 'a poll while New is open keeps what was typed: ' +
    JSON.stringify(byId(list(), 'nt_label') && byId(list(), 'nt_label').value));
  const fold = findAll(list(), e => e.tagName === 'BUTTON' && e.classList.contains('head'))[0];
  fold.dispatch('click', { target: fold });
  assert(tp.termOpenNew === false && byId(list(), 'nt_label') !== typedLabel, 'folding New lets it go');
  // the open form is one node, and it is the list's first after any note
  tp.termOpenNew = true; tp.renderTerm();
  const fresh = byId(list(), 'nt_label');
  assert(fresh && fresh.value === '', 'and opening it again starts empty');
  tp.termOpenNew = false; tp.renderTerm();

  // a hostile link and a name with markup stay text.
  const hostile = '<img src=x onerror=alert(1)>';
  page.terminals.push({ id: 't-mark', kind: 'shell', name: '<b>x</b>', title: '<i>t</i>', cwd: '/',
    started: ago(5 * H), holder: null, viewers: [], ended: false });
  await tp.signOut();
  await settle();
  location.hash = '#' + hostile;
  const before = fetches.length;
  await tp.signIn('driver', 'right', () => {});
  await settle();
  const after = fetches.slice(before);
  assert(!after.some(f => /img|%3C|</i.test(f.path)), 'a hostile link asks the server for nothing: ' + JSON.stringify(after.map(f => f.path)));
  assert(!after.some(f => f.path.startsWith('/v1/terminals/')), 'not even for a session: ' + JSON.stringify(after.map(f => f.path)));
  assert(tp.termId === null, 'and opens nothing: ' + tp.termId);
  assert(page.listeners.hashchange.length === 1, 'a second sign-in does not listen for hashchange twice');
  const note = findAll(list(), e => e.classList && e.classList.contains('warn') && e.tagName === 'P')[0];
  assert(note && note.textContent === 'that link names no session here' && list().children[0] === note,
    'it says so, at the top of the list: ' + (note && note.textContent));
  const all = [list(), nodes['#tstrip'], nodes['#tinfo'], nodes['#tscreen']].flatMap(n => [n, ...n._all()]);
  assert(!all.some(e => e.tagName === 'IMG'), 'no node is an image');
  assert(!all.some(e => String(e.textContent || '').includes('onerror') ||
    Object.values(e.attrs || {}).some(v => String(v).includes('onerror'))), 'and the link is not echoed anywhere');
  location.hash = '#also-not-a-session';
  page.listeners.hashchange.forEach(f => f());
  await settle();
  assert(tp.termId === null && fetches.slice(before).every(f => !f.path.startsWith('/v1/terminals/')),
    'a hashchange to an unknown id fetches and opens nothing either');
  const m = row('t-mark');
  const mb = m && findAll(m, e => e.tagName === 'B')[0];
  assert(mb && mb.textContent === '<b>x</b>' && mb.children.length === 0,
    'a name with markup is a row whose b holds it as text: ' + (mb && JSON.stringify(mb.children)));
  assert(m.textContent.includes('<i>t</i>') && !findAll(m, e => e.tagName === 'I').length, 'and so is a title');
  // The kit records every string the page tried to have parsed; none may be
  // anything but the header.
  assert(page.markup.length === 0, 'nothing but the header went to innerHTML: ' + JSON.stringify(page.markup));
  // Choosing a session clears the note.
  await tp.termSelect('t-mark');
  await settle();
  assert(!findAll(list(), e => e.tagName === 'P' && e.classList.contains('warn')).length, 'opening a session clears the note');

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
