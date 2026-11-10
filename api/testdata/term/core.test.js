// core.js and a held session: the state frame, the keyboard's connection,
// and a session the page may not attach to, which it never opens a socket
// to however long it waits, and says why.
const kit = require(process.env.T + '/term/kit.js');
const { assert, byId, findAll, btn } = kit;

const CLOCK = kit.CLOCK;
const held = { id: 'h-live', kind: 'account', label: 'work', cwd: '/src', started: new Date(CLOCK - 60000).toISOString(),
  cols: 80, rows: 24, holder: null, viewers: [], offset: 0, ended: false, recording: false,
  held: true, state: 'live', attachable: true, status: 'running', waiting_for: '', title: '', name: 'work',
  account: { id: 1, label: 'a@x', provider: 'claude' } };
const stopped = { id: 'h-stop', kind: 'account', label: 'old', cwd: '/src', started: new Date(CLOCK - 7200000).toISOString(),
  cols: 80, rows: 24, holder: null, viewers: [], offset: 0, ended: true, exit: 0, recording: false,
  held: true, attachable: false, state: 'interrupted', status: 'stopped', resumable: true, name: 'old',
  account: { id: 1, label: 'a@x', provider: 'claude' } };
const foreign = { id: 'h-foreign', kind: 'account', label: '', cwd: '/src', started: new Date(CLOCK - 600000).toISOString(),
  cols: 80, rows: 24, holder: null, viewers: [], offset: 0, ended: false, recording: false,
  held: true, attachable: false, made_by: '1.4.0-beta.3', state: 'live', status: 'running', name: '<b>x</b>',
  account: { id: 1, label: 'a@x', provider: 'claude' } };

(async () => {
  const page = await kit.load({ terminals: [held, stopped, foreign], role: 'control', signedIn: true });
  const { nodes, sockets, fakes } = page;
  const tp = page.tp;
  const strip = () => nodes['#tstrip'];
  const screen = () => nodes['#tscreen'];
  const lastSock = () => sockets[sockets.length - 1];

  // termWhyNot, rule by rule.
  assert(tp.termWhyNot(null) === null && tp.termWhyNot({}) === null && tp.termWhyNot({ attachable: true }) === null,
    'a session that does not say it cannot be attached to can be');
  assert(tp.termWhyNot({ attachable: false, made_by: '9.0.0', ended: true }) ===
    'made by rota 9.0.0, which this rota cannot attach to; stop it, or attach with that version',
    'a foreign holder says which rota made it: ' + tp.termWhyNot({ attachable: false, made_by: '9.0.0' }));
  assert(tp.termWhyNot({ attachable: false, ended: true, state: 'interrupted' }) ===
    'this session has stopped: interrupted; its screen is not kept',
    'a stopped one says how it stopped: ' + tp.termWhyNot({ attachable: false, ended: true, state: 'interrupted' }));
  assert(tp.termWhyNot({ attachable: false }) === 'this session cannot be attached to here', 'and anything else says so');

  // 1. A held session's hello says which connection holds its keyboard, and
  //    a keyboard frame moves it.
  let before = sockets.length;
  await tp.termSelect('h-live');
  await page.settle();
  assert(sockets.length === before + 1, 'a held session that can be attached to is: ' + (sockets.length - before));
  const sock = lastSock();
  sock.open();
  sock.feed({ type: 'hello', id: 'h-live', kind: 'account', cols: 80, rows: 24, offset: 0,
    holder: 'driver', holder_conn: 'c-1', you: { name: 'driver', role: 'control', conn: 'c-1' }, viewers: [] });
  assert(tp.termHeld === true && tp.termHolderConn === 'c-1', 'hello says the holder is c-1: ' + tp.termHolderConn);
  assert(tp.termHoldsHere() === true, 'which is this connection');
  sock.feed({ type: 'keyboard', holder: 'driver', holder_conn: 'c-2' });
  assert(tp.termHolderConn === 'c-2' && tp.termHoldsHere() === false,
    'a keyboard frame moves it to c-2, another tab of the same person: ' + tp.termHolderConn);

  // 2. A state frame fills termState, keeps the description in step, and
  //    redraws what says it.
  const drawn = {};
  for (const f of ['renderTermStrip', 'renderTermInfo', 'renderTermDock', 'renderTermList']) {
    const was = tp[f];
    drawn[f] = 0;
    tp[f] = (...a) => { drawn[f]++; return was(...a); };
  }
  sock.feed({ type: 'state', status: 'needs_you', waiting_for: 'permission', title: 'Editing x', name: 'work' });
  assert(JSON.stringify(tp.termState) === JSON.stringify({ status: 'needs_you', waiting_for: 'permission', title: 'Editing x', name: 'work' }),
    'the state frame is termState: ' + JSON.stringify(tp.termState));
  assert(tp.termInfo.status === 'needs_you' && tp.termInfo.waiting_for === 'permission' && tp.termInfo.title === 'Editing x',
    'and the description says the same: ' + JSON.stringify(tp.termInfo));
  assert(tp.termWaiting() === true, 'the session is waiting on its person');
  for (const f of Object.keys(drawn)) assert(drawn[f] >= 1, f + ' redraws for a state frame');
  sock.feed({ type: 'state', status: 'running' });
  assert(JSON.stringify(tp.termState) === JSON.stringify({ status: 'running', waiting_for: '', title: '', name: '' }),
    'a field the frame leaves out is empty, not the last one: ' + JSON.stringify(tp.termState));
  assert(tp.termInfo.waiting_for === '' && tp.termWaiting() === false, 'and nothing is waited on any more');

  // A frame of a type the page does not know changes nothing and throws nothing.
  const snap = () => JSON.stringify([tp.termState, tp.termInfo, tp.termStatus, tp.termHolder, tp.termHolderConn, tp.termOffset]);
  const was = snap();
  for (const f of [{ type: 'nonsense', status: 'needs_you' }, { type: 'constructor' }, { status: 'quiet' }, null, [1, 2], 'null', '"state"']) {
    sock.feed(f);
  }
  assert(snap() === was, 'a frame the page does not know changes nothing: ' + snap());

  // 3. A stopped or foreign session opens no socket, however long the page waits.
  // Whatever termActions draws is drawn once, in the stopped screen, so no
  // id it gives a control is ever on the page twice.
  tp.termActions = d => tp.el('button', { id: 'term_resume', class: 'btn', text: 'Resume ' + d.id });
  const resumes = () => ['#tscreen', '#tinfo', '#tstrip', '#tlist', '#tdock']
    .map(s => findAll(nodes[s], e => e.attrs && e.attrs.id === 'term_resume').length);
  assert(resumes().join() === '0,0,0,0,0', 'a session that is attached shows no stopped screen: ' + resumes());

  const waitLong = async () => {
    for (let i = 0; i < 3; i++) { page.fire(); page.advance(60000); await page.settle(); }
    // A retry and a sign-in's pick-up both go through termConnect.
    tp.termConnect(); tp.termRetry(); page.fire(); await page.settle();
  };

  before = sockets.length;
  const views = fakes.length;
  await tp.termSelect('h-stop');
  await page.settle();
  assert(sockets.length === before, 'a stopped session is not attached to');
  await waitLong();
  assert(sockets.length === before, 'not after three minutes either: ' + (sockets.length - before) + ' sockets');
  assert(fakes.length === views, 'and no emulator is made for it');
  assert(tp.termStatus === 'stopped', 'its status is stopped: ' + tp.termStatus);
  assert(screen().textContent.includes('interrupted') && screen().textContent.includes('its screen is not kept'),
    'the screen says how it stopped: ' + screen().textContent);
  assert(screen().textContent.includes('old'), 'and which one it is: ' + screen().textContent);
  const pill = findAll(strip(), e => e.classList && e.classList.contains('pill'))[0];
  assert(pill && pill.textContent === 'stopped' && pill.classList.contains('bad'),
    'the strip says stopped, as bad: ' + strip().textContent);
  assert(!btn(strip(), 'Take the keyboard') && !btn(strip(), 'Ask for the keyboard'),
    'and offers no keyboard: ' + strip().textContent);
  const empty = findAll(screen(), e => e.classList && e.classList.contains('empty'));
  assert(empty.length === 1, 'the screen is one .empty block: ' + empty.length);
  assert(resumes().join() === '1,0,0,0,0', 'with the actions in it, and nowhere else: ' + resumes());
  assert(byId(empty[0], 'term_resume').textContent === 'Resume h-stop', 'for this session');

  before = sockets.length;
  await tp.termSelect('h-foreign');
  await page.settle();
  await waitLong();
  assert(sockets.length === before, 'a foreign holder is not attached to: ' + (sockets.length - before) + ' sockets');
  assert(tp.termStatus === 'stopped' && strip().textContent.includes('stopped'), 'and says stopped: ' + strip().textContent);
  assert(screen().textContent.includes('made by rota 1.4.0-beta.3'), 'the screen says who made it: ' + screen().textContent);
  // A name with markup in it is a name: one B, holding the text, no child.
  const name = findAll(screen(), e => e.tagName === 'B');
  assert(name.length === 1 && name[0].textContent === '<b>x</b>' && name[0].children.length === 0,
    'a name with markup in it stays text: ' + JSON.stringify(name.map(n => [n.textContent, n.children.length])));
  assert(resumes().join() === '1,0,0,0,0', 'its actions are drawn once too: ' + resumes());

  // A well-formed id the server does not have answers 404: no socket, and
  // the screen says so.
  before = sockets.length;
  await tp.termSelect('0123456789abcdef');
  await page.settle();
  await waitLong();
  assert(sockets.length === before, 'an id the server does not have opens no socket: ' + (sockets.length - before));
  assert(tp.termStatus === 'stopped', 'its status is stopped: ' + tp.termStatus);
  assert(screen().textContent.includes('no session 0123456789abcdef on this server'),
    'the screen says there is no such session: ' + screen().textContent);
  assert(resumes().join() === '0,0,0,0,0', 'with nothing to do to it: ' + resumes());

  // And a session that can be attached to still is, after all that, with
  // the stopped screen gone.
  before = sockets.length;
  await tp.termSelect('h-live');
  await page.settle();
  assert(sockets.length === before + 1 && tp.termStatus === 'connecting', 'a live one is attached to again: ' + tp.termStatus);
  assert(!findAll(screen(), e => e.classList && e.classList.contains('empty')).length && resumes().join() === '0,0,0,0,0',
    'and the stopped screen is gone: ' + screen().textContent);

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
