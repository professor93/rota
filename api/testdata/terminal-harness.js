// Drives the real terminal page against the real schema a server with its
// terminal group on serves, in Node, on a minimal DOM. xterm is never
// loaded: the page keeps it behind one factory, and what is put there
// instead is a terminal that only remembers what it was shown.
const fs = require('fs');
const T = process.env.T;
const { nodes, sockets } = require(T + '/dom.js');
const schema = JSON.parse(fs.readFileSync(T + '/schema-terminal.json', 'utf8'));
const accountsDoc = JSON.parse(fs.readFileSync(T + '/accounts.json', 'utf8'));

// The terminals this fake server holds. The ended one is listed first on
// purpose: putting what is running at the top is the page's own doing.
const CLOCK = 1758000000000;
let terminalsDoc = { terminals: [
  { id: 't-done', kind: 'shell', label: '', cwd: '/', started: new Date(CLOCK - 3600000).toISOString(),
    cols: 80, rows: 24, holder: null, viewers: [], offset: 10, ended: true, exit: 2, recording: false },
  { id: 't-live', kind: 'account', label: 'fintech', cwd: '/src/api',
    started: new Date(CLOCK - 720000).toISOString(), cols: 120, rows: 40, holder: 'alice',
    viewers: [{ name: 'alice', role: 'control', since: '2026-01-01T00:00:00Z' },
              { name: 'looker', role: 'watch', since: '2026-01-01T00:00:00Z' }],
    offset: 4096, ended: false, recording: true,
    account: { id: 1, label: 'a@x', provider: 'claude' },
    token_until: new Date(CLOCK + 1800000).toISOString() },
] };
let lastTerminal = null;
const deletes = [];
let sessionDoc = null;
let signInRole = 'control';

const jsonAPI = async (path, init = {}) => {
  const reply = doc => ({
    ok: true, status: 200, headers: { get: () => 'application/json' },
    text: async () => JSON.stringify(doc), json: async () => doc,
  });
  const refuse = (status, doc) => ({
    ok: false, status, headers: { get: () => 'application/json' },
    text: async () => JSON.stringify(doc), json: async () => doc,
  });
  if (path === '/v1/session') {
    if (init.method === 'POST') {
      const body = JSON.parse(init.body);
      if (body.password !== 'right') return refuse(401, { error: 'that name and password do not go together' });
      sessionDoc = { name: body.name, role: signInRole, via: 'user', expires: '2030-01-01T00:00:00Z' };
      return reply(sessionDoc);
    }
    if (init.method === 'DELETE') { sessionDoc = null; return reply({ signed_out: true }); }
    if (sessionDoc) return reply(sessionDoc);
    if (init.headers && init.headers.authorization) return reply({ name: 'token', role: 'control', via: 'token' });
    return refuse(401, { error: 'invalid or missing bearer token' });
  }
  if (path.startsWith('/v1/terminals')) {
    if (init.method === 'POST') {
      lastTerminal = JSON.parse(init.body || '{}');
      const made = { id: 't-new', kind: lastTerminal.kind || 'account', label: lastTerminal.label || '',
        cwd: lastTerminal.cwd || '', started: new Date(CLOCK).toISOString(), cols: 80, rows: 24,
        holder: null, viewers: [], offset: 0, ended: false, recording: false };
      terminalsDoc.terminals.push(made);
      return reply(made);
    }
    if (init.method === 'DELETE') { deletes.push(path); return reply({ ok: true }); }
    const id = path.slice('/v1/terminals'.length).replace(/^\//, '').split('?')[0];
    if (id) {
      const one = terminalsDoc.terminals.find(t => t.id === id);
      return one ? reply(one) : refuse(404, { error: 'no terminal ' + id });
    }
    return reply(terminalsDoc);
  }
  if (path.includes('/schema')) return reply(schema);
  if (path.includes('/accounts')) return reply(accountsDoc);
  return reply({ ok: true });
};
globalThis.fetch = jsonAPI;

// The page is two scripts: the one both of rota's pages load first, and the
// terminal page's own. A browser gives them one scope between them, so they
// are run here the same way.
const shared = fs.readFileSync(T + '/page.js', 'utf8');
const src = fs.readFileSync(T + '/term.js', 'utf8');
const tp = new Function(shared + '\n' + src + `
;return {
  signIn, signOut, watching, renderLink, refreshTerminals, renderTerm, renderTermStrip, renderTermInfo,
  termSelect, termTick, termFit, splitArgs, enterTerminal,
  set makeTerminal(v){makeTerminal=v}, set loadXterm(v){loadXterm=v},
  set termNow(v){termNow=v}, set termLater(v){termLater=v},
  set termOpenNew(v){termOpenNew=v},
  get link(){return $("#otherpage")},
  get me(){return me}, get schema(){return schema},
  get terms(){return terms}, get termId(){return termId},
  get termOffset(){return termOffset}, get termHolder(){return termHolder},
  get termStatus(){return termStatus}, get termEnded(){return termEnded},
  get termSock(){return termSock},
};`)();

const findAll = (root, pred) => (root._all ? root._all().filter(pred) : []);
const byId = (root, id) => findAll(root, e => e.attrs && e.attrs.id === id)[0];
const assert = (cond, msg) => { if (!cond) { console.error('FAILED:', msg); process.exit(1); } };
const settle = () => new Promise(r => setTimeout(r, 5));

// A terminal that only remembers: the page must be drivable without an
// emulator, or none of this could be tested outside a browser.
const fakes = [];
let fitSize = { cols: 96, rows: 30 };
const fakeTerminal = () => {
  const t = {
    writes: [], sizes: [], fits: 0, disposed: false, data: null,
    write(b) { this.writes.push(b); },
    onData(cb) { this.data = cb; },
    resize(c, r) { this.sizes.push([c, r]); },
    fit() { this.fits++; return fitSize; },
    dispose() { this.disposed = true; },
    text() { return this.writes.map(b => new TextDecoder().decode(b)).join(''); },
  };
  fakes.push(t);
  return t;
};
tp.makeTerminal = async () => fakeTerminal();
tp.loadXterm = async () => {};

// The clock and the timers, so a ten-second countdown takes no time at all.
let clock = CLOCK;
const timers = [];
tp.termNow = () => clock;
tp.termLater = (fn, ms) => { timers.push({ fn, ms }); };
const fire = () => { for (const t of timers.splice(0)) t.fn(); };

const strip = () => nodes['#tstrip'];
const info = () => nodes['#tinfo'].textContent;
const btn = (host, text) => findAll(host, e => e.tagName === 'BUTTON' && e.textContent.includes(text))[0];
const lastSock = () => sockets[sockets.length - 1];
const screen = () => fakes[fakes.length - 1];

(async () => {
  // 1. signed out, this page is the sign-in the other page shows.
  assert(nodes['#termshell'].hidden === true, 'nobody is shown a terminal before signing in');
  assert(byId(nodes['#gate'], 'signin_name'), 'the page asks for a name and a password');
  await tp.signIn('driver', 'right', () => {});
  await settle();
  assert(tp.me && tp.me.role === 'control', 'signing in makes this page that principal');
  assert(nodes['#termshell'].hidden === false && !nodes['#gate'].children.length,
    'and the terminal is what is shown afterwards');
  assert(tp.link.hidden === false && tp.link.attrs.href === '/playground',
    'with the way back to the playground, which this server serves: ' + JSON.stringify(tp.link.attrs));

  // 2. what there is, running first and ended after.
  const rows = () => findAll(nodes['#tlist'], e => e.classList && e.classList.contains('trow'));
  assert(rows().length === 2, 'both terminals are listed: ' + rows().length);
  assert(rows()[0].attrs['data-id'] === 't-live', 'what is running comes first');
  assert(rows()[0].textContent.includes('fintech') && rows()[0].textContent.includes('#1 a@x'),
    'a row says what it is and who is paying: ' + rows()[0].textContent);
  assert(rows()[0].textContent.includes('alice has the keyboard') && rows()[0].textContent.includes('2 watching'),
    'and who is typing and how many are looking: ' + rows()[0].textContent);
  assert(rows()[1].textContent.includes('ended, code 2'), 'an ended one says how: ' + rows()[1].textContent);

  // 3. New terminal posts what the form holds, split the way a shell would.
  assert(JSON.stringify(tp.splitArgs('--resume "an id" -x')) === '["--resume","an id","-x"]',
    'arguments split on spaces and quotes: ' + JSON.stringify(tp.splitArgs('--resume "an id" -x')));
  tp.termOpenNew = true; tp.renderTerm();
  const acct = byId(nodes['#tlist'], 'nt_account');
  const label = byId(nodes['#tlist'], 'nt_label');
  const args = byId(nodes['#tlist'], 'nt_args');
  const cwd = byId(nodes['#tlist'], 'nt_cwd');
  assert(acct && label && args && cwd, 'a control principal is offered a new terminal');
  assert(byId(nodes['#tlist'], 'nt_shell'), 'and a plain shell, because this server allows one');
  acct.value = '1'; label.value = 'audit'; args.value = "--resume 'an id'"; cwd.value = '/src';
  const start = byId(nodes['#tlist'], 'nt_start');
  start.dispatch('click', { target: start });
  await settle(); await settle();
  assert(lastTerminal && lastTerminal.account === 1 && lastTerminal.label === 'audit' &&
    lastTerminal.cwd === '/src' && JSON.stringify(lastTerminal.args) === '["--resume","an id"]',
    'the new terminal posts what the form holds: ' + JSON.stringify(lastTerminal));
  assert(tp.termId === 't-new', 'and the page opens what it started');

  // 4. choosing one attaches to it, and hello fills the panel.
  const before4 = sockets.length;
  await tp.termSelect('t-live');
  assert(sockets.length === before4 + 1, 'choosing another terminal opens one socket');
  const sock = lastSock();
  assert(sock.url === 'ws://127.0.0.1:8787/v1/terminals/t-live/ws',
    'a first attach asks for the whole scrollback: ' + sock.url);
  assert(sock.binaryType === 'arraybuffer', 'and for the output as bytes, not as text');
  sock.open();
  sock.feed({ type: 'hello', id: 't-live', kind: 'account', cols: 120, rows: 40, offset: 4096,
    holder: 'alice', you: { name: 'driver', role: 'control', conn: 'c-1' },
    viewers: [{ name: 'alice', role: 'control' }, { name: 'driver', role: 'control' }] });
  assert(tp.termOffset === 4096, 'hello says where the bytes start: ' + tp.termOffset);
  assert(tp.termStatus === 'live' && strip().textContent.includes('live'), 'the strip says the socket is live');
  assert(strip().textContent.includes('120×40'), 'and how big the terminal is: ' + strip().textContent);
  assert(info().includes('#1 a@x') && info().includes('claude'), 'the panel names the account: ' + info());
  assert(info().includes('/src/api') && info().includes('12m ago'), 'the folder and the age: ' + info());
  assert(info().includes('alice'), 'who holds the keyboard');
  assert(info().includes('Recording') && info().includes('on'), 'and whether it is being kept');
  assert(info().includes('driver'), 'and who is watching');

  // 5. output arrives as bytes, and moves the offset on by its own length.
  sock.feedBytes('hello world');
  assert(screen().text() === 'hello world', 'the bytes reach the terminal: ' + JSON.stringify(screen().text()));
  assert(screen().writes[0] instanceof Uint8Array, 'as bytes, not as a string');
  assert(tp.termOffset === 4096 + 11, 'and the offset follows them: ' + tp.termOffset);

  // 6. a key pressed by somebody who is not holding goes nowhere.
  const sent6 = sock.sent.length;
  screen().data('l');
  assert(sock.sent.length === sent6, 'a key nobody may send is not sent to be refused');
  assert(findAll(strip(), e => e.classList && e.classList.contains('flash')).length === 1,
    'the keyboard control says no, once');
  fire();

  // 7. the three states of the keyboard, and what each offers.
  assert(btn(strip(), 'Ask for the keyboard'), 'somebody else has it: it can be asked for');
  sock.feed({ type: 'keyboard', holder: null });
  await settle();
  assert(btn(strip(), 'Take the keyboard') && !btn(strip(), 'Ask for the keyboard'),
    'nobody has it: it can be taken');
  sock.feed({ type: 'keyboard', holder: 'driver' });
  await settle();
  assert(btn(strip(), 'Release') && strip().textContent.includes('hold the keyboard'),
    'this page has it: it can be let go of');

  // and now what is typed is sent, as bytes and never as text
  screen().data('ls\r');
  const typed = sock.sent[sock.sent.length - 1];
  assert(typed instanceof Uint8Array, 'what was typed is a binary frame: ' + typeof typed);
  assert(new TextDecoder().decode(typed) === 'ls\r', 'and it is what was typed');
  assert(!sock.sent.some(f => typeof f === 'string' && f.includes('ls')),
    'a text frame is never how input travels: ' + JSON.stringify(sock.sent.filter(f => typeof f === 'string')));

  // 8. the holder is asked for it, and answers.
  sock.feed({ type: 'claim_request', by: 'bob', id: 'c-9' });
  assert(strip().textContent.includes('bob') && btn(strip(), 'Grant') && btn(strip(), 'Deny'),
    'the holder is shown the request: ' + strip().textContent);
  const deny = btn(strip(), 'Deny');
  deny.dispatch('click', { target: deny });
  let said = JSON.parse(sock.sent[sock.sent.length - 1]);
  assert(said.type === 'deny' && said.to === 'c-9', 'Deny answers it by its own id: ' + JSON.stringify(said));
  sock.feed({ type: 'claim_request', by: 'bob', id: 'c-9' });
  const grant = btn(strip(), 'Grant');
  grant.dispatch('click', { target: grant });
  said = JSON.parse(sock.sent[sock.sent.length - 1]);
  assert(said.type === 'grant' && said.to === 'c-9', 'and Grant hands it over: ' + JSON.stringify(said));

  // 9. asking for it, and the ten seconds before it may be forced.
  sock.feed({ type: 'keyboard', holder: 'alice' });
  await settle();
  const ask = btn(strip(), 'Ask for the keyboard');
  ask.dispatch('click', { target: ask });
  assert(JSON.parse(sock.sent[sock.sent.length - 1]).type === 'claim', 'asking is a claim frame');
  assert(strip().textContent.includes('asked'), 'and the wait is shown: ' + strip().textContent);
  assert(!btn(strip(), 'Force'), 'forcing is not offered while the wait is on');
  clock += 4000; tp.termTick();
  assert(!btn(strip(), 'Force'), 'nor part way through it');
  clock += 7000; tp.termTick();
  const force = btn(strip(), 'Force');
  assert(force, 'ten seconds unanswered and it may be forced: ' + strip().textContent);
  force.dispatch('click', { target: force });
  const forced = JSON.parse(sock.sent[sock.sent.length - 1]);
  assert(forced.type === 'claim' && forced.force === true, 'which says so: ' + JSON.stringify(forced));

  // 10. somebody else's size is shown as it is, not as this window's.
  sock.feed({ type: 'resized', cols: 100, rows: 30 });
  assert(JSON.stringify(screen().sizes[screen().sizes.length - 1]) === '[100,30]',
    'a terminal this page is not typing into is the size of the session: ' + JSON.stringify(screen().sizes));
  assert(strip().textContent.includes('100×30'), 'and the strip says so');

  // 11. falling behind is 1013: come back at once, from where this page got to.
  const before11 = sockets.length;
  sock.close(1013);
  assert(sockets.length === before11 + 1, 'a 1013 is reattached to at once');
  const back = lastSock();
  assert(back.url === 'ws://127.0.0.1:8787/v1/terminals/t-live/ws?since=' + tp.termOffset,
    'with the offset this page was actually shown: ' + back.url);

  // 12. and output that was not kept is said where it happened.
  back.open();
  back.feed({ type: 'hello', id: 't-live', kind: 'account', cols: 100, rows: 30, offset: 9000,
    holder: 'alice', you: { name: 'driver', role: 'control', conn: 'c-2' }, viewers: [] });
  back.feed({ type: 'gap', from: 4107, to: 9000 });
  assert(screen().text().includes('4893 bytes were not kept'),
    'the gap is written into the terminal: ' + JSON.stringify(screen().text().slice(-80)));
  assert(tp.termOffset === 9000, 'and the offset is what hello said: ' + tp.termOffset);

  // 13. a connection that simply broke comes back with a wait, not at once.
  const before13 = sockets.length;
  back.close(1006);
  assert(sockets.length === before13, 'a broken connection is not retried the same instant');
  assert(strip().textContent.includes('reconnecting in 1s'), 'and the wait is said: ' + strip().textContent);
  fire();
  assert(sockets.length === before13 + 1, 'and then it is retried');
  const third = lastSock();
  assert(third.url.includes('since=9000'), 'from the same place: ' + third.url);
  third.open();
  third.feed({ type: 'hello', id: 't-live', kind: 'account', cols: 100, rows: 30, offset: 9000,
    holder: null, you: { name: 'driver', role: 'control', conn: 'c-3' }, viewers: [] });

  // 14. the login inside the terminal, and the hour at which it is a warning.
  assert(/lapses in \d+m/.test(info()), 'the panel says when the login inside lapses: ' + info());
  assert(findAll(nodes['#tinfo'], e => e.classList && e.classList.contains('warn')).length === 1,
    'and under an hour that is a warning');
  terminalsDoc.terminals.find(t => t.id === 't-live').token_until = new Date(clock + 5 * 3600000).toISOString();
  await tp.refreshTerminals();
  assert(info().includes('lapses in 5h'), 'further off it is only a fact: ' + info());
  assert(findAll(nodes['#tinfo'], e => e.classList && e.classList.contains('warn')).length === 0,
    'and not a warning');

  // 15. choosing another terminal says goodbye to this one, cleanly, once.
  await tp.termSelect('t-done');
  assert(JSON.stringify(third.closes) === '[1000]',
    'leaving a terminal says goodbye once: ' + JSON.stringify(third.closes));
  assert(screen() !== fakes[0] && fakes.some(f => f.disposed), 'and the emulator goes with it');

  // 16. the CLI ending shows the code and stops all of it.
  await tp.termSelect('t-live');
  const last = lastSock();
  last.open();
  last.feed({ type: 'hello', id: 't-live', kind: 'account', cols: 100, rows: 30, offset: 9000,
    holder: null, you: { name: 'driver', role: 'control', conn: 'c-4' }, viewers: [] });
  last.feed({ type: 'exit', code: 3 });
  await settle();
  assert(tp.termEnded === 3 && strip().textContent.includes('ended, code 3'),
    'the exit is shown with its code: ' + strip().textContent);
  assert(info().includes('exited, code 3'), 'and in the panel: ' + info());
  assert(!btn(strip(), 'Take the keyboard') && !btn(strip(), 'Release'),
    'a terminal that has ended has no keyboard to hold');
  const after16 = sockets.length;
  last.close(1006);
  fire();
  assert(sockets.length === after16, 'and nothing reconnects to a terminal that is over');

  // 17. a watcher sees all of it and may touch none of it.
  signInRole = 'watch';
  await tp.signOut();
  assert(nodes['#termshell'].hidden === true, 'signing out puts the sign-in back');
  await tp.signIn('looker', 'right', () => {});
  await settle();
  assert(tp.watching(), 'the page knows it is watching');
  assert(!byId(nodes['#tlist'], 'nt_start'), 'a watcher is not offered a new terminal');
  assert(nodes['#tlist'].textContent.includes('watching: this sign-in cannot change anything'),
    'and is told why: ' + nodes['#tlist'].textContent.slice(0, 120));
  await tp.termSelect('t-live');
  const wsock = lastSock();
  wsock.open();
  wsock.feed({ type: 'hello', id: 't-live', kind: 'account', cols: 120, rows: 40, offset: 0,
    holder: 'alice', you: { name: 'looker', role: 'watch', conn: 'c-5' }, viewers: [] });
  assert(strip().textContent.includes('watching'), 'a watcher is told what they are: ' + strip().textContent);
  assert(!btn(strip(), 'Ask for the keyboard') && !btn(strip(), 'Take the keyboard'),
    'and is offered no keyboard at all');
  assert(!byId(nodes['#tinfo'], 'term_kill'), "nor a way to end somebody else's terminal");
  const wsent = wsock.sent.length;
  screen().data('x');
  assert(wsock.sent.length === wsent, "and a watcher's keystroke goes nowhere");

  console.log('TERMINAL_OK');
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
