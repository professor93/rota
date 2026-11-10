// The kit every terminal-page test runs on. It loads page.js and the files
// terminal.html names under /assets/term/, in the order the page names
// them, into one scope, the way a browser gives classic scripts one scope
// between them; and around that scope it puts dom.js, a fake server, a fake
// clock and, unless asked otherwise, a fake emulator.
//
//   const kit = require(process.env.T + '/term/kit.js');
//   const page = await kit.load({ terminals: [], role: 'control', signedIn: true });
//   page.tp.termSelect('t-1'); page.tp.termOffset; page.tp.termNow = () => 0;
const fs = require('fs');
const T = process.env.T;

const CLOCK = 1758000000000;

const assert = (cond, msg) => { if (!cond) { console.error('FAILED:', msg); process.exit(1); } };
const findAll = (root, pred) => (root && root._all ? root._all().filter(pred) : []);
const byId = (root, id) => findAll(root, e => e.attrs && e.attrs.id === id)[0];
const btn = (root, text) => findAll(root, e => e.tagName === 'BUTTON' && e.textContent.includes(text))[0];
const ok = () => console.log('TERM_TEST_OK');

// The page's own files, in the order terminal.html loads them.
function termScripts() {
  const html = fs.readFileSync(T + '/terminal.html', 'utf8');
  const out = [];
  for (const m of html.matchAll(/<script src="\/assets\/term\/([^"?]+\.js)(?:\?[^"]*)?"><\/script>/g)) out.push(m[1]);
  return out;
}

async function load(opts = {}) {
  const o = Object.assign({ terminals: [], role: 'control', signedIn: true, coarse: false, hash: '', realView: false }, opts);
  // A fresh DOM for every page: dom.js sets its globals when it is required.
  delete require.cache[require.resolve(T + '/dom.js')];
  const { nodes, sockets } = require(T + '/dom.js');
  const schema = JSON.parse(fs.readFileSync(T + '/schema-terminal.json', 'utf8'));
  const accountsDoc = JSON.parse(fs.readFileSync(T + '/accounts.json', 'utf8'));

  const page = {
    nodes, sockets, fakes: [], terminals: o.terminals, fetches: [], routes: [],
    role: o.role, fitSize: { cols: 96, rows: 30 }, proposeSize: { cols: 96, rows: 30 },
    history: [], clipboard: [], listeners: {}, timers: [], clock: CLOCK,
    storage: globalThis.localStorage,
  };
  let sessionDoc = null;

  // What the browser around the page is.
  globalThis.location.hash = o.hash;
  globalThis.location.pathname = '/terminal';
  const history = { replaceState: (...args) => { page.history.push(args); } };
  globalThis.history = history;
  window.history = history;
  window.location = globalThis.location;
  window.matchMedia = q => ({ matches: !!o.coarse && String(q).includes('pointer:coarse'),
    media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {} });
  window.addEventListener = (type, fn) => { (page.listeners[type] = page.listeners[type] || []).push(fn); };
  // Node has a navigator of its own with no setter, so it is replaced rather
  // than assigned.
  Object.defineProperty(globalThis, 'navigator', { configurable: true, writable: true,
    value: { clipboard: { writeText: async text => { page.clipboard.push(text); } } } });

  // The server. A route is tried before the defaults, newest first; one that
  // answers nothing (undefined) lets the next one answer.
  page.route = (method, prefix, fn) => { page.routes.unshift({ method, prefix, fn }); };
  const answer = (status, doc) => ({
    ok: status >= 200 && status < 300, status, headers: { get: () => 'application/json' },
    text: async () => JSON.stringify(doc), json: async () => doc,
  });
  const defaults = (method, path, body, init) => {
    if (path === '/v1/session') {
      if (method === 'POST') {
        if (!body || body.password !== 'right') return { status: 401, doc: { error: 'that name and password do not go together' } };
        sessionDoc = { name: body.name, role: page.role, via: 'user', expires: '2030-01-01T00:00:00Z' };
        return { status: 200, doc: sessionDoc };
      }
      if (method === 'DELETE') { sessionDoc = null; return { status: 200, doc: { signed_out: true } }; }
      if (sessionDoc) return { status: 200, doc: sessionDoc };
      if (init.headers && init.headers.authorization) return { status: 200, doc: { name: 'token', role: 'control', via: 'token' } };
      return { status: 401, doc: { error: 'invalid or missing bearer token' } };
    }
    if (path.startsWith('/v1/terminals')) {
      if (method === 'POST' && path === '/v1/terminals') {
        const b = body || {};
        const made = { id: 't-new', kind: b.kind || 'account', label: b.label || '',
          cwd: b.cwd || '', started: new Date(CLOCK).toISOString(), cols: 80, rows: 24,
          holder: null, viewers: [], offset: 0, ended: false, recording: false };
        page.terminals.push(made);
        return { status: 200, doc: made };
      }
      if (method === 'DELETE') return { status: 200, doc: { ok: true } };
      const id = path.slice('/v1/terminals'.length).replace(/^\//, '').split('?')[0];
      if (id) {
        const one = page.terminals.find(t => t.id === decodeURIComponent(id));
        return one ? { status: 200, doc: one } : { status: 404, doc: { error: 'no terminal ' + id } };
      }
      return { status: 200, doc: { terminals: page.terminals } };
    }
    if (path.includes('/schema')) return { status: 200, doc: schema };
    if (path.includes('/accounts')) return { status: 200, doc: accountsDoc };
    return { status: 200, doc: { ok: true } };
  };
  globalThis.fetch = async (path, init = {}) => {
    const method = init.method || 'GET';
    let body = init.body;
    if (typeof body === 'string') { try { body = JSON.parse(body); } catch {} }
    page.fetches.push({ method, path, body });
    for (const r of page.routes) {
      if (r.method !== method || !path.startsWith(r.prefix)) continue;
      const got = await r.fn(path, body);
      if (got) return answer(got.status === undefined ? 200 : got.status, got.doc);
    }
    const got = defaults(method, path, body, init);
    return answer(got.status, got.doc);
  };

  // The page: page.js and its own files, as one scope, with an accessor that
  // reads and assigns its names through direct eval.
  const files = ['page.js', ...termScripts().map(f => 'assets/term/' + f)];
  const src = files.map(f => fs.readFileSync(T + '/' + f, 'utf8')).join('\n');
  const scope = new Function(src + '\n;return (__k, __set, __v) => __set ? eval(__k + " = __v") : eval(__k);')();

  const ident = /^[A-Za-z_$][\w$]*$/;
  page.tp = new Proxy({}, {
    get(_, k) {
      if (typeof k !== 'string' || k === 'then' || !ident.test(k)) return undefined;
      return scope(k, false);
    },
    set(_, k, v) {
      if (typeof k !== 'string' || !ident.test(k)) throw new Error('not a name: ' + String(k));
      scope(k, false); // throws for a name the page does not have, rather than making a global
      scope(k, true, v);
      return true;
    },
  });
  const tp = page.tp;

  // The clock and the timers, so a ten-second countdown takes no time.
  tp.termNow = () => page.clock;
  tp.termLater = (fn, ms) => { page.timers.push({ fn, ms }); };
  page.fire = () => { for (const t of page.timers.splice(0)) t.fn(); };
  page.advance = ms => { page.clock += ms; };
  page.settle = () => new Promise(r => setTimeout(r, 5));

  // A terminal that only remembers what it was shown and asked.
  if (!o.realView) {
    const fake = () => {
      const t = {
        writes: [], sizes: [], fits: 0, fonts: [], disposed: false, data: null, cursorKeys: false,
        write(b) { this.writes.push(b); },
        onData(cb) { this.data = cb; },
        resize(c, r) { this.sizes.push([c, r]); },
        fit() { this.fits++; return page.fitSize; },
        propose() { return page.proposeSize; },
        font(px) { this.fonts.push(px); },
        appCursor() { return this.cursorKeys; },
        dispose() { this.disposed = true; },
        text() { return this.writes.map(b => new TextDecoder().decode(b)).join(''); },
      };
      page.fakes.push(t);
      return t;
    };
    tp.makeTerminal = async () => fake();
    tp.loadXterm = async () => {};
  }

  await page.settle();
  if (o.signedIn) {
    await tp.signIn('driver', 'right', () => {});
    await page.settle();
  }
  return page;
}

module.exports = { load, assert, byId, findAll, btn, ok, CLOCK };
