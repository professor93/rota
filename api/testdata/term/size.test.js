// The size of a held session: the page never resizes it by itself. It is
// drawn at the session's size, scaled to the window's width from 13 px down
// to 7 px or shown at its actual size and panned, and it changes size only
// when somebody allowed to presses Fit, having said yes when others watch.
const kit = require(process.env.T + '/term/kit.js');
const { assert, byId } = kit;

const CLOCK = kit.CLOCK;
const terminals = [
  { id: '0123456789abcdef', kind: 'account', held: true, name: 'fintech', label: 'fintech', cwd: '/src/api',
    started: new Date(CLOCK - 60000).toISOString(), cols: 120, rows: 40, holder: null, viewers: [],
    offset: 0, ended: false, recording: false, account: { id: 1, label: 'a@x', provider: 'claude' } },
  { id: 'fedcba9876543210', kind: 'account', label: 'plain', cwd: '/src',
    started: new Date(CLOCK - 60000).toISOString(), cols: 80, rows: 24, holder: null, viewers: [],
    offset: 0, ended: false, recording: false, account: { id: 1, label: 'a@x', provider: 'claude' } },
];
const HELD = terminals[0].id, PLAIN = terminals[1].id;

(async () => {
  const page = await kit.load({ terminals });
  const { nodes, sockets, fakes } = page;
  const tp = page.tp;
  const strip = () => nodes['#tstrip'];
  const screen = () => fakes[fakes.length - 1];
  const sock = () => sockets[sockets.length - 1];
  const frames = (s, from = 0) => s.sent.slice(from).filter(f => typeof f === 'string').map(f => JSON.parse(f));
  const resizes = s => frames(s).filter(f => f.type === 'resize');
  const font = () => { const f = screen().fonts; return f.length ? f[f.length - 1] : 13; };
  const lastSize = () => JSON.stringify(screen().sizes[screen().sizes.length - 1]);
  const click = b => b.dispatch('click', { target: b });
  const me = conn => ({ name: 'driver', role: 'control', conn });
  const hello = (over = {}) => Object.assign({ type: 'hello', id: HELD, kind: 'account', cols: 120, rows: 40,
    offset: 0, holder: 'driver', holder_conn: 'c-1', you: me('c-1'),
    viewers: [{ name: 'driver', role: 'control' }] }, over);
  const open = async (id, h) => {
    await tp.termSelect(id);
    await page.settle();
    sock().open();
    sock().feed(h);
    await page.settle();
    return sock();
  };
  const fontsSane = what => assert(screen().fonts.every(f => Number.isInteger(f) && f >= 7 && f <= 13),
    what + ': every font is a whole number from 7 to 13: ' + JSON.stringify(screen().fonts));

  // 1. A held session never sends a resize: not after hello, not from a
  // window resized, not while this tab holds its keyboard.
  page.proposeSize = { cols: 60, rows: 30 };
  page.fitSize = { cols: 96, rows: 30 };
  const s1 = await open(HELD, hello());
  page.fire();
  assert(tp.termHoldsHere(), 'this tab holds the keyboard of the held session');
  assert(resizes(s1).length === 0, 'hello sends no resize for a held session: ' + JSON.stringify(frames(s1)));
  for (const f of page.listeners.resize || []) f();
  tp.termFit();
  page.fire();
  assert(resizes(s1).length === 0, 'nor does the window changing size: ' + JSON.stringify(frames(s1)));
  assert(lastSize() === '[120,40]', 'it is drawn at the session\'s size: ' + lastSize());
  assert(screen().fits === 0, 'and it is never fitted by itself: ' + screen().fits);

  // 2. Scaling: 60 columns fit at 13 px.
  assert(font() === 7, 'a 120-column session in 60 columns\' room is drawn at 7 px, 6 clamped up: ' + font());
  fontsSane('120 columns');
  assert(tp.termScaleFont(60, 13, 120) === 7, 'termScaleFont clamps 6 up to 7');
  assert(tp.termScaleFont(200, 13, 80) === 13, 'and never goes above 13');
  assert(strip().textContent.includes('120×40'), 'the strip says the session\'s size: ' + strip().textContent);
  const big = () => byId(strip(), 'term_big');
  assert(big() && big().textContent === 'larger than this window: drawn smaller',
    'and that it is larger than this window, drawn smaller: ' + strip().textContent);
  s1.feed({ type: 'resized', cols: 100, rows: 40 });
  assert(lastSize() === '[100,40]' && font() === 7, 'a 100-column one gets 7 px: ' + lastSize() + ' ' + font());
  page.proposeSize = { cols: 92, rows: 30 };
  s1.feed({ type: 'resized', cols: 100, rows: 30 });
  assert(font() === 11, '92 columns\' room at 13 px for 100 columns gets 11 px: ' + JSON.stringify(screen().fonts));
  tp.termScale(); tp.termScale(); tp.termFit();
  assert(font() === 11, 'and asking again does not drift: ' + JSON.stringify(screen().fonts));
  fontsSane('100 columns');

  // The toggle: actual size, remembered on this device.
  let zoom = byId(strip(), 'term_zoom');
  assert(zoom && zoom.textContent === 'Actual size', 'a held session larger than the window offers its actual size');
  click(zoom);
  assert(font() === 13, 'Actual size draws it at 13 px: ' + font());
  assert(page.storage.getItem('rota.zoom') === 'actual', 'and is remembered: ' + page.storage.getItem('rota.zoom'));
  assert(big() && big().textContent === 'larger than this window', 'the note no longer says smaller: ' + strip().textContent);
  s1.feed({ type: 'resized', cols: 110, rows: 30 });
  assert(font() === 13, 'a resize keeps the actual size: ' + font());
  zoom = byId(strip(), 'term_zoom');
  assert(zoom && zoom.textContent === 'Scaled', 'which can be turned back');
  click(zoom);
  assert(page.storage.getItem('rota.zoom') === 'fit' && font() < 13, 'to scaled: ' + font());

  // The note goes once the session fits.
  s1.feed({ type: 'resized', cols: 90, rows: 30 });
  assert(font() === 13, 'a session that fits is drawn at 13 px: ' + JSON.stringify(screen().fonts));
  assert(!big() && !byId(strip(), 'term_zoom'), 'with no note and no toggle: ' + strip().textContent);
  assert(resizes(s1).length === 0, 'and still nothing resized it: ' + JSON.stringify(frames(s1)));

  // A resized made by another device of the same person is followed here.
  s1.feed({ type: 'keyboard', holder: 'driver', holder_conn: 'c-other' });
  assert(tp.termIHold() && !tp.termHoldsHere(), 'the same name holds it on another connection');
  s1.feed({ type: 'resized', cols: 150, rows: 30 });
  assert(lastSize() === '[150,30]', 'the view follows that size: ' + lastSize());
  assert(font() === 7, 'and is scaled for it: ' + font());
  page.fire();
  assert(resizes(s1).length === 0, 'and nothing answers it with a resize: ' + JSON.stringify(frames(s1)));

  // 3. Fit.
  const fit = () => byId(strip(), 'term_fit');
  page.fitSize = { cols: 90, rows: 28 };
  // Held on another connection of the same name: claim, then resize.
  let from = s1.sent.length;
  assert(fit() && !fit().disabled, 'Fit is offered to the holder\'s name on another connection');
  assert(fit().attrs.title === 'make this session the size of this window, for everyone watching',
    'and says what it does: ' + fit().attrs.title);
  click(fit());
  let said = frames(s1, from);
  assert(JSON.stringify(said) === '[{"type":"claim"},{"type":"resize","cols":90,"rows":28,"fit":true}]',
    'Fit claims and then sizes the session: ' + JSON.stringify(said));
  assert(screen().fonts.includes(13), 'measured at 13 px: ' + JSON.stringify(screen().fonts));

  // Held here: only the resize.
  s1.feed({ type: 'keyboard', holder: 'driver', holder_conn: 'c-1' });
  from = s1.sent.length;
  click(fit());
  said = frames(s1, from);
  assert(JSON.stringify(said) === '[{"type":"resize","cols":90,"rows":28,"fit":true}]',
    'held here, Fit only sizes the session: ' + JSON.stringify(said));
  s1.feed({ type: 'resized', cols: 90, rows: 28 });
  assert(lastSize() === '[90,28]', 'and the view follows the session when it says so: ' + lastSize());

  // Free: claim, then resize.
  s1.feed({ type: 'keyboard', holder: null, holder_conn: '' });
  from = s1.sent.length;
  assert(fit() && !fit().disabled, 'Fit is offered with the keyboard free');
  click(fit());
  said = frames(s1, from);
  assert(JSON.stringify(said) === '[{"type":"claim"},{"type":"resize","cols":90,"rows":28,"fit":true}]',
    'with the keyboard free, Fit claims it first: ' + JSON.stringify(said));

  // 4. a Fit under another person's keyboard, or declined, sends nothing.
  s1.feed({ type: 'keyboard', holder: 'alice', holder_conn: 'c-a' });
  assert(fit() && fit().disabled, 'Fit is disabled under another person\'s keyboard');
  assert(fit().attrs.title === 'alice holds the keyboard; only its holder fits the session',
    'and says whose it is: ' + fit().attrs.title);
  from = s1.sent.length;
  click(fit());
  page.fire();
  assert(s1.sent.length === from, 'a tap on it anyway sends nothing: ' + JSON.stringify(frames(s1, from)));

  s1.feed({ type: 'keyboard', holder: 'driver', holder_conn: 'c-1' });
  const before = lastSize();
  let asked = null;
  globalThis.confirm = q => { asked = q; return false; };
  // One other watching is one other, not "1 others".
  s1.feed({ type: 'viewers', list: [{ name: 'driver', role: 'control' }, { name: 'bob', role: 'watch' }] });
  from = s1.sent.length;
  click(fit());
  page.fire();
  assert(asked === 'Make fintech 90×28 for the 1 other watching it?', 'Fit asks the one other watching: ' + asked);
  assert(s1.sent.length === from, 'a no sends nothing: ' + JSON.stringify(frames(s1, from)));
  s1.feed({ type: 'viewers', list: [{ name: 'driver', role: 'control' }, { name: 'alice', role: 'control' },
    { name: 'bob', role: 'watch' }] });
  asked = null;
  from = s1.sent.length;
  click(fit());
  page.fire();
  assert(asked === 'Make fintech 90×28 for the 2 others watching it?', 'Fit asks first when others watch: ' + asked);
  assert(s1.sent.length === from, 'a no sends nothing: ' + JSON.stringify(frames(s1, from)));
  assert(lastSize() === before, 'and leaves the view at the session\'s size: ' + lastSize());
  globalThis.confirm = q => { asked = q; return true; };
  click(fit());
  said = frames(s1, from);
  assert(JSON.stringify(said) === '[{"type":"resize","cols":90,"rows":28,"fit":true}]', 'a yes sizes it: ' + JSON.stringify(said));

  // 5. degenerate sizes.
  page.proposeSize = null;
  const fontsBefore = screen().fonts.length;
  let threw = null;
  try { tp.termScale(); s1.feed({ type: 'resized', cols: 200, rows: 50 }); tp.renderTermStrip(); } catch (e) { threw = e; }
  assert(!threw, 'a hidden container throws nothing: ' + (threw && threw.stack));
  assert(screen().fonts.length === fontsBefore, 'and leaves the font alone: ' + JSON.stringify(screen().fonts));
  page.proposeSize = { cols: 60, rows: 30 };
  s1.feed({ type: 'resized', cols: 1000, rows: 40 });
  assert(font() === 7, 'a 1000-column session gets 7 px: ' + font());
  fontsSane('1000 columns');
  page.fitSize = { cols: 0, rows: 0 };
  globalThis.confirm = () => true;
  from = s1.sent.length;
  click(fit());
  page.fire();
  assert(s1.sent.length === from, 'a Fit to 0×0 sends nothing: ' + JSON.stringify(frames(s1, from)));
  assert(strip().textContent.includes('this window has no room to fit the session to'),
    'and says why: ' + strip().textContent);
  assert(lastSize() === '[1000,40]', 'the view stays at the session\'s size: ' + lastSize());
  // From 7 px, where the room is rounded down to whole cells, a session that
  // fits at 13 px is still drawn at 13 px, with no note.
  s1.feed({ type: 'resized', cols: 60, rows: 30 });
  assert(font() === 13 && !big(), 'a session that just fits goes back to 13 px: ' + JSON.stringify(screen().fonts));

  // 6. A terminal that is not held keeps today's fitting, and has no Fit.
  page.fitSize = { cols: 96, rows: 30 };
  const s2 = await open(PLAIN, { type: 'hello', id: PLAIN, kind: 'account', cols: 80, rows: 24, offset: 0,
    holder: 'driver', you: me('c-9'), viewers: [] });
  page.fire();
  assert(!tp.termHeld, 'this one is not held');
  assert(JSON.stringify(resizes(s2)) === '[{"type":"resize","cols":96,"rows":30}]',
    'it is fitted to the window and says so: ' + JSON.stringify(frames(s2)));
  assert(!byId(strip(), 'term_fit') && !byId(strip(), 'term_zoom'), 'with no Fit and no toggle: ' + strip().textContent);
  assert(screen().fonts.length === 0, 'and its font is never changed: ' + JSON.stringify(screen().fonts));

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
