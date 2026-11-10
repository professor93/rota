// The emulator's adapter, view.js, run as it is rather than replaced: it
// loads the vendored files in order, under this server's version, and makes
// a terminal that measures wide characters by Unicode 11, as a held
// session's own screen does. xterm itself is a fake here that behaves as
// 5.5 does where it matters: term.unicode throws unless the proposed API was
// allowed, and a Unicode version nobody registered cannot be made active.
const kit = require(process.env.T + '/term/kit.js');
const { assert } = kit;

(async () => {
  const page = await kit.load({ realView: true });
  const tp = page.tp;

  // Signing in starts loadXterm, which appends one file at a time, each
  // after the last has loaded.
  const head = document.head;
  assert(tp.schema && tp.schema.version, 'the schema says a version');
  const v = '?v=' + encodeURIComponent(tp.schema.version);
  const want = [
    ['LINK', 'href', '/assets/xterm/xterm.css' + v],
    ['SCRIPT', 'src', '/assets/xterm/xterm.js' + v],
    ['SCRIPT', 'src', '/assets/xterm/addon-fit.js' + v],
    ['SCRIPT', 'src', '/assets/xterm/addon-unicode11.js' + v],
  ];
  let done = false;
  const loading = tp.loadXterm().then(() => { done = true; });
  assert(tp.termAssets && tp.loadXterm() === tp.termAssets, 'a second call waits on the load sign-in started');
  for (let i = 0; i < want.length; i++) {
    await page.settle();
    assert(head.children.length === i + 1,
      `file ${i + 1} is asked for only after file ${i} has loaded: ${head.children.length} appended`);
    const [tag, attr, url] = want[i];
    const n = head.children[i];
    assert(n.tagName === tag && n.attrs[attr] === url, `file ${i + 1} is ${url}: ${n.tagName} ${JSON.stringify(n.attrs)}`);
    assert(!done, 'loading is not done before ' + url + ' has loaded');
    n.dispatch('load');
  }
  await loading;
  assert(done && head.children.length === want.length, 'loading is done after the fourth file, and asks for no fifth');
  assert(tp.loadXterm() === tp.loadXterm(), 'and it happens once');

  // The fake xterm.
  let proposal = () => ({ cols: 100, rows: 40 });
  const made = [];
  globalThis.Terminal = class {
    constructor(opts) {
      this.options = Object.assign({}, opts);
      this.versions = ['6'];
      this.active = '6';
      this.modes = { applicationCursorKeysMode: false };
      this.cols = 80; this.rows = 24;
      made.push(this);
    }
    get unicode() {
      if (!this.options.allowProposedApi) throw new Error('You must set the allowProposedApi option to true to use proposed API');
      const t = this;
      return {
        register(p) { t.versions.push(p.version); },
        get activeVersion() { return t.active; },
        set activeVersion(x) { if (!t.versions.includes(x)) throw new Error('unknown Unicode version "' + x + '"'); t.active = x; },
      };
    }
    loadAddon(a) { a.activate(this); }
    open() {}
    write() {}
    onData() {}
    resize() {}
    dispose() {}
  };
  globalThis.FitAddon = { FitAddon: class { activate() {} fit() {} proposeDimensions() { return proposal(); } } };
  globalThis.Unicode11Addon = { Unicode11Addon: class { activate(t) { t.unicode.register({ version: '11' }); } } };

  const view = await tp.makeTerminal(tp.el('div'));
  assert(made.length === 1, 'one terminal is made');
  const term = made[0];
  assert(term.options.allowProposedApi === true, 'the terminal is made with the proposed API allowed');
  assert(term.unicode.activeVersion === '11', 'and measures by Unicode 11: ' + term.unicode.activeVersion);

  // propose says what would fit, or nothing.
  const p = view.propose();
  assert(p && p.cols === 100 && p.rows === 40, 'propose says what fits: ' + JSON.stringify(p));
  proposal = () => { throw new Error('the container is not shown'); };
  assert(view.propose() === null, 'a measurement that throws is null');
  proposal = () => undefined;
  assert(view.propose() === null, 'no measurement is null');
  proposal = () => ({ cols: NaN, rows: NaN });
  assert(view.propose() === null, 'a measurement with no columns is null');

  // font draws it smaller, and a font xterm refuses is not the page's end.
  view.font(9);
  assert(term.options.fontSize === 9, 'font(9) draws at 9 px: ' + term.options.fontSize);
  Object.defineProperty(term.options, 'fontSize', { set() { throw new Error('refused'); }, get() { return 9; } });
  view.font(8);

  // appCursor is what the CLI asked of the arrow keys.
  assert(view.appCursor() === false, 'the arrows are normal until asked otherwise');
  term.modes.applicationCursorKeysMode = true;
  assert(view.appCursor() === true, 'and application keys once the CLI asks');

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
