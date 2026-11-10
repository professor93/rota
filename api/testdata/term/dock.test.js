// The dock: whether it shows, the composer that types a whole message into a
// held session once it is ready, and the quick keys a CLI asks for. A wrong
// byte typed into a live CLI is a real harm, and so is a message lost or sent
// twice, so every send here is counted and every key's bytes are exact.
const kit = require(process.env.T + '/term/kit.js');
const { assert, byId, findAll } = kit;

const H1 = '0123456789abcdef', H2 = 'fedcba9876543210', PLAIN = '1111222233334444';
const started = new Date(kit.CLOCK - 60000).toISOString();
const session = (id, extra) => Object.assign({ id, kind: 'account', label: '', cwd: '/src', started,
  cols: 80, rows: 24, holder: null, viewers: [], offset: 0, ended: false, recording: false,
  account: { id: 1, label: 'a@x', provider: 'claude' } }, extra);

(async () => {
  const page = await kit.load({ coarse: true, terminals: [
    session(H1, { held: true, label: 'fintech <b>x</b>' }),
    session(H2, { held: true, label: 'second' }),
    session(PLAIN, { kind: 'shell', account: undefined }),
  ] });
  const { nodes, sockets, fakes } = page;
  const tp = page.tp;
  const dock = () => nodes['#tdock'];
  const strip = () => nodes['#tstrip'];
  const inDock = id => byId(dock(), id);
  const box = () => inDock('dock_text');
  const send = () => inDock('dock_send');
  const say = () => inDock('dock_say');
  const toggle = () => byId(strip(), 'dock_toggle');
  const sock = () => sockets[sockets.length - 1];
  const click = async n => { assert(n, 'there is something to press'); n.dispatch('click'); await page.settle(); };
  const type = text => { box().value = text; box().dispatch('input'); };
  const same = (got, want, what) =>
    assert(JSON.stringify(got) === JSON.stringify(want), what + ': ' + JSON.stringify(got));

  // The server's side of a send: each answer is taken in turn, and with none
  // left a send is typed.
  const answers = [];
  page.route('POST', '/v1/terminals/', (path, body) => {
    if (!path.endsWith('/messages')) return undefined;
    const next = answers.shift();
    return next ? next(path, body) : { status: 200, doc: { ok: true } };
  });
  const posts = () => page.fetches.filter(f => f.method === 'POST' && f.path.endsWith('/messages'));
  const later = () => { let res; const p = new Promise(r => { res = r; }); return { p, res }; };

  const hello = (id, extra) => Object.assign({ type: 'hello', id, kind: 'account', cols: 80, rows: 24,
    offset: 0, holder: null, you: { name: 'driver', role: 'control', conn: 'c-1' }, viewers: [] }, extra);
  async function open(id, extra) {
    await tp.termSelect(id);
    await page.settle();
    const s = sock();
    s.open();
    s.feed(hello(id, extra));
    return s;
  }
  // A frame the page sent, as the server reads it: text as text, and bytes
  // as the string they spell.
  const frames = s => s.sent.map(d => (typeof d === 'string' ? 'text:' + d
    : 'bytes:' + JSON.stringify(Array.from(d, b => String.fromCharCode(b)).join(''))));

  // ------------------------------------------------------------ showing it --
  let s = await open(H1, { holder_conn: null });
  assert(tp.termHeld === true, 'the first session is held');
  assert(dock().hidden === false, 'on a coarse pointer the dock shows by itself');
  assert(toggle() && toggle().textContent === 'Composer', 'and the strip offers the toggle: ' + strip().textContent);
  assert(toggle().attrs['aria-pressed'] === 'true', 'which says the dock is shown');

  window.matchMedia = q => ({ matches: false, media: q });
  tp.renderTerm();
  assert(dock().hidden === true, 'with a fine pointer it does not');
  assert(toggle().attrs['aria-pressed'] === 'false', 'and the toggle says so');
  page.storage.setItem('rota.dock', 'on');
  tp.renderTerm();
  assert(dock().hidden === false && tp.termDockShown() === true, 'unless this device asked for it');
  window.matchMedia = q => ({ matches: true, media: q });
  page.storage.setItem('rota.dock', 'off');
  tp.renderTerm();
  assert(dock().hidden === true && tp.termDockShown() === false, 'and a device that said off keeps it off');
  page.storage.removeItem('rota.dock');
  tp.renderTerm();
  assert(dock().hidden === false, 'a device that said nothing follows its pointer again');

  await click(toggle());
  assert(page.storage.getItem('rota.dock') === 'off', 'the toggle stores the opposite of what was shown: ' +
    page.storage.getItem('rota.dock'));
  assert(dock().hidden === true && toggle().attrs['aria-pressed'] === 'false', 'and hides the dock');
  await click(toggle());
  assert(page.storage.getItem('rota.dock') === 'on' && dock().hidden === false, 'and a second press shows it');

  // Storage that refuses every read and write is no reason to fail: the
  // toggle still works for as long as the page is open.
  const kept = globalThis.localStorage;
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, get() { throw new Error('denied'); } });
  tp.renderTerm();
  await click(toggle());
  assert(dock().hidden === true, 'with storage refused, the toggle still hides the dock');
  await click(toggle());
  assert(dock().hidden === false, 'and shows it');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, writable: true, value: kept });
  assert(page.storage.getItem('rota.dock') === 'on', 'and what was stored before is still there');

  // A watcher, and an ended session, have nothing to type with.
  s.feed(hello(H1, { holder_conn: null, you: { name: 'looker', role: 'watch', conn: 'c-9' } }));
  assert(dock().hidden === true && !toggle(), 'a watcher gets no dock and no toggle: ' + strip().textContent);
  s.feed(hello(H1, { holder_conn: null }));
  assert(dock().hidden === false, 'a controller does');

  // ------------------------------------------------------------- composer --
  assert(box() && box().tagName === 'TEXTAREA', 'a held session has the composer');
  same([box().attrs.rows, box().attrs.autocapitalize, box().attrs.spellcheck, box().attrs.placeholder],
    ['2', 'sentences', 'true', 'Write a message; it is typed in once the session is ready'], 'the box');
  assert(send() && send().textContent === 'Send', 'and Send');
  assert(send().disabled === true, 'which is disabled while there is nothing to send');
  type('  \n\t ');
  assert(send().disabled === true, 'and while there is only whitespace');
  const node = box();
  s.feed({ type: 'keyboard', holder: null, holder_conn: null });
  assert(box() === node, 'a redraw keeps the same box, so a phone keeps its keyboard up');

  // Exactly the text, with only its trailing whitespace gone.
  type('hello world  \n\n');
  assert(send().disabled === false, 'text makes Send pressable');
  let gate = later();
  answers.push(() => gate.p);
  await click(send());
  same(posts().map(p => p.body), [{ text: 'hello world' }], 'Send posts exactly the trimmed text');
  assert(posts()[0].path === '/v1/terminals/' + H1 + '/messages', 'to the session: ' + posts()[0].path);
  assert(send().textContent === 'Sending…' && send().disabled === true, 'Send says it is sending and is disabled');
  await click(send());
  box().dispatch('keydown', { key: 'Enter', ctrlKey: true });
  await page.settle();
  assert(posts().length === 1, 'a second press, or Ctrl+Enter, during a send posts nothing: ' + posts().length);
  gate.res({ status: 200, doc: { ok: true } });
  await page.settle();
  assert(box().value === '' && !tp.termDrafts.get(H1), 'a 200 clears the box and the draft');
  assert(send().textContent === 'Send', 'and Send is back');

  // Many lines, tabs, a leading space, markup, quotes, an escape in what was
  // pasted, and text beyond ASCII all go as they are.
  const pasted = '  first line\n\tsecond, "quoted" \\ back\n<b>not markup</b>\x1b[31m\n héllo 👋   end';
  type(pasted + ' \n \n');
  await click(send());
  same(posts()[1].body, { text: pasted }, 'a paste goes byte for byte, only its trailing whitespace trimmed');
  assert(box().value === '', 'and is cleared once typed');

  // 409 waiting offers Send anyway, which forces.
  answers.push(() => ({ status: 409, doc: { error: 'the session is waiting for its person; force sends anyway', reason: 'waiting' } }));
  type('approve it');
  await click(send());
  assert(say().textContent === 'the session is waiting for its person; force sends anyway',
    'a waiting refusal says the server\'s sentence: ' + say().textContent);
  assert(box().value === 'approve it', 'and keeps the text');
  assert(inDock('dock_force') && inDock('dock_force').textContent === 'Send anyway', 'and offers Send anyway');
  await click(inDock('dock_force'));
  same(posts()[3].body, { text: 'approve it', force: true }, 'Send anyway posts force');
  assert(box().value === '' && !inDock('dock_force') && say().textContent === '', 'and once typed it is all gone');

  // Any other refusal keeps the text and says why.
  answers.push(() => ({ status: 409, doc: { error: 'alice holds the keyboard of this session', reason: 'keyboard', holder: 'alice' } }));
  type('mine');
  await click(send());
  assert(say().textContent === 'alice holds the keyboard of this session', 'a keyboard refusal says the server\'s sentence');
  assert(box().value === 'mine' && !inDock('dock_force'), 'keeps the text, and offers no force');
  answers.push(() => ({ status: 504, doc: { error: 'the session did not become ready for the text in time; nothing was typed', reason: 'timeout' } }));
  await click(send());
  assert(say().textContent.includes('nothing was typed') && box().value === 'mine', 'and so does a timeout');

  // The keys that send.
  const before = posts().length;
  let prevented = 0;
  const key = ev => box().dispatch('keydown', Object.assign({ key: 'Enter', preventDefault() { prevented++; } }, ev));
  key({ isComposing: true, ctrlKey: true });
  key({});
  key({ shiftKey: true });
  await page.settle();
  assert(posts().length === before && prevented === 0, 'plain Enter, or Ctrl+Enter while composing, is a new line');
  key({ ctrlKey: true });
  await page.settle();
  assert(posts().length === before + 1 && prevented === 1, 'Ctrl+Enter sends');
  type('by meta');
  key({ metaKey: true });
  await page.settle();
  assert(posts().length === before + 2 && posts()[before + 1].body.text === 'by meta', '⌘+Enter sends');

  // Over 256 KiB of request, nothing is posted; at exactly 256 KiB it is.
  const n = posts().length;
  type('a'.repeat(256 * 1024));
  await click(send());
  assert(posts().length === n && say().textContent === 'a message is at most 256 KiB', 'over the limit nothing is posted');
  type('"'.repeat(140 * 1024));
  await click(send());
  assert(posts().length === n, 'nor when escaping takes it over: ' + posts().length);
  type('é'.repeat(130 * 1024));
  await click(send());
  assert(posts().length === n, 'nor when UTF-8 does');
  assert(box().value === 'é'.repeat(130 * 1024), 'and the text stays');
  const fits = 'b'.repeat(256 * 1024 - JSON.stringify({ text: '' }).length);
  type(fits);
  await click(send());
  assert(posts().length === n + 1 && posts()[n].body.text === fits, 'a request of exactly 256 KiB goes');

  // A send the network loses is not sent again (Review Focus 1).
  answers.push(() => { throw new Error('the network went'); });
  type('once only');
  await click(send());
  const lost = posts().length;
  assert(box().value === 'once only', 'a send the network loses keeps its text');
  assert(say().textContent === 'the message may not have been sent; look at the screen before sending it again',
    'and says it may not have gone: ' + say().textContent);
  for (let i = 0; i < 3; i++) { page.fire(); page.advance(60000); await page.settle(); }
  tp.renderTerm();
  await page.settle();
  assert(posts().length === lost, 'and nothing sends it again by itself: ' + (posts().length - lost));
  assert(send().disabled === false && send().textContent === 'Send', 'the person may choose to send it again');

  // A draft survives switching sessions, and a send's answer lands on the
  // session it was for.
  type('draft one');
  s = await open(H2, { holder_conn: null });
  assert(box().value === '', 'another session has its own box: ' + JSON.stringify(box().value));
  box().value = 'draft two';           // set without an input event, as some keyboards do
  s = await open(H1, { holder_conn: null });
  assert(box().value === 'draft one', 'a draft survives switching away and back: ' + box().value);
  s = await open(H2, { holder_conn: null });
  assert(box().value === 'draft two', 'and so does the other one');
  gate = later();
  answers.push(() => gate.p);
  await click(send());
  s = await open(H1, { holder_conn: null });
  assert(send().disabled === false, 'a send out for another session does not hold this one');
  gate.res({ status: 200, doc: { ok: true } });
  await page.settle();
  assert(box().value === 'draft one', 'its answer leaves this session\'s box alone');
  assert(!tp.termDrafts.get(H2), 'and clears the draft it was for');
  s = await open(H2, { holder_conn: null });
  assert(box().value === '', 'which is empty when opened again');

  // Text typed while a send was out is not lost when it is answered.
  type('first');
  gate = later();
  answers.push(() => gate.p);
  await click(send());
  type('first and more');
  gate.res({ status: 200, doc: { ok: true } });
  await page.settle();
  assert(box().value === ' and more', 'what was added during a send stays: ' + JSON.stringify(box().value));
  gate = later();
  answers.push(() => gate.p);
  type('alpha');
  await click(send());
  type('beta');
  gate.res({ status: 200, doc: { ok: true } });
  await page.settle();
  assert(box().value === 'beta', 'and an edit made during a send is kept whole: ' + box().value);
  s = await open(H1, { holder_conn: null });
  type('');

  // ------------------------------------------------------------ the keys --
  const keys = () => findAll(inDock('dock_keys'), e => e.tagName === 'BUTTON');
  same(keys().map(k => [k.attrs['data-key'], k.textContent, k.attrs['aria-label'] || '']), [
    ['1', '1', ''], ['2', '2', ''], ['3', '3', ''], ['esc', 'Esc', ''], ['enter', '⏎', 'Enter'], ['tab', 'Tab', ''],
    ['up', '↑', 'Up'], ['down', '↓', 'Down'], ['left', '←', 'Left'], ['right', '→', 'Right'], ['ctrl-c', '^C', 'Ctrl-C'],
  ], 'the quick keys, in order');
  const want = { 1: '1', 2: '2', 3: '3', esc: '\x1b', enter: '\r', tab: '\t',
    up: '\x1b[A', down: '\x1b[B', left: '\x1b[D', right: '\x1b[C', 'ctrl-c': '\x03' };
  const bytes = v => 'bytes:' + JSON.stringify(v);
  const press = async name => click(keys().find(k => k.attrs['data-key'] === name));

  // Held by this connection: each key is one binary frame of exactly its
  // bytes. Esc and Ctrl-C must be a frame of one byte, since that is what a
  // send holding the session lets through, and an arrow must never be split
  // into an Esc the server would take as one.
  s.feed({ type: 'keyboard', holder: 'driver', holder_conn: 'c-1' });
  for (const [name, b] of Object.entries(want)) {
    s.sent.length = 0;
    await press(name);
    same(frames(s), [bytes(b)], name + ' sends its bytes as one frame');
  }
  fakes[fakes.length - 1].cursorKeys = true;
  for (const [name, b] of Object.entries({ up: '\x1bOA', down: '\x1bOB', left: '\x1bOD', right: '\x1bOC' })) {
    s.sent.length = 0;
    await press(name);
    same(frames(s), [bytes(b)], name + ' follows the cursor-key mode');
  }
  fakes[fakes.length - 1].cursorKeys = false;
  same([tp.termKeyBytes('up', true), tp.termKeyBytes('up', false), tp.termKeyBytes('constructor', false),
    tp.termKeyBytes('toString', true), tp.termKeyBytes('', false)], ['\x1bOA', '\x1b[A', '', '', ''],
  'termKeyBytes knows its keys and nothing else');

  // The keyboard free: claim it, then the bytes, in that order.
  s.feed({ type: 'keyboard', holder: null, holder_conn: null });
  s.sent.length = 0;
  await press('esc');
  same(frames(s), ['text:{"type":"claim"}', bytes('\x1b')], 'a free keyboard is claimed before the key');

  // The same person on another connection holds it: the server lets that
  // person type, so the key goes with no claim.
  s.feed({ type: 'keyboard', holder: 'driver', holder_conn: 'c-2' });
  s.sent.length = 0;
  await press('2');
  same(frames(s), [bytes('2')], 'the same name on another connection sends the bytes with no claim');

  // A quick key under another person's keyboard types nothing (Review Focus 2).
  s.feed({ type: 'keyboard', holder: 'alice', holder_conn: 'c-7' });
  s.sent.length = 0;
  for (const name of Object.keys(want)) await press(name);
  same(frames(s), [], 'a quick key under another person\'s keyboard sends nothing');
  assert(strip().textContent.includes('alice holds the keyboard'), 'and says who holds it: ' + strip().textContent);
  assert(tp.termNudge === true, 'and flashes');
  page.fire();

  // ---------------------------------------------------- when it asks you --
  s.feed({ type: 'keyboard', holder: null, holder_conn: null });
  const order = () => dock().children.map(c => c.attrs.id);
  same(order(), ['dock_compose', 'dock_keys'], 'the composer comes first');
  assert(!dock().classList.contains('asks'), 'and the dock does not ask');
  tp.termState = { status: 'needs_you', waiting_for: 'permission', title: '', name: '' };
  tp.renderTermDock();
  assert(dock().classList.contains('asks'), 'a session waiting on you makes the dock ask');
  same(order(), ['dock_ask', 'dock_keys', 'dock_compose'], 'the note, then the keys, then the composer');
  assert(dock().children[0].textContent === 'fintech <b>x</b> is waiting for a permission: answer with these keys',
    'the note names the session and what it waits for, as text: ' + dock().children[0].textContent);
  assert(box() === node, 'and the box is the same box');
  tp.termState = null;
  tp.termInfo.status = 'needs_you';
  tp.renderTermDock();
  assert(dock().children[0].textContent === 'fintech <b>x</b> is waiting for you: answer with these keys',
    'a description that needs you, with nothing more said, waits for you: ' + dock().children[0].textContent);
  tp.termInfo.status = 'running';
  tp.renderTermDock();
  same(order(), ['dock_compose', 'dock_keys'], 'and once answered the composer comes first again');

  // ----------------------------------------------------- an ended session --
  s.feed({ type: 'exit', code: 0 });
  assert(dock().hidden === true && !toggle(), 'an ended session gets no dock and no toggle');

  // ------------------------------------------------ a terminal not held --
  s = await open(PLAIN, { kind: 'shell', holder: 'driver' });
  assert(tp.termHeld === false, 'the shell is not held');
  assert(dock().hidden === false && !box() && !send(), 'it has the dock but no composer');
  assert(inDock('dock_compose').textContent === 'the composer types into held sessions; this terminal takes keys directly',
    'and says why: ' + inDock('dock_compose').textContent);
  s.sent.length = 0;
  await press('ctrl-c');
  await press('enter');
  same(frames(s), [bytes('\x03'), bytes('\r')], 'its keys still work');

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
