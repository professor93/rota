// Drives the real playground script against the real schema this server
// serves, in Node, on a minimal DOM. A page is otherwise the one part of a
// Go project no test touches, and the part most likely to break silently.
const fs = require('fs');
const T = process.env.T;
const { nodes } = require(T + '/dom.js');
const schema = JSON.parse(fs.readFileSync(T + '/schema.json', 'utf8'));
const accountsDoc = JSON.parse(fs.readFileSync(T + '/accounts.json', 'utf8'));

// What the server sends when asked what the CLIs are doing. The shapes are
// the ones sessions.Scan produces, including the ones with something missing:
// an instance nobody can attribute, and a conversation nobody owns.
const sessionsDoc = {
  instances: [
    { kind: 'cli', provider: 'claude', account: 2, label: 'a@x', dir: '/src/api', pid: 4242, session: 'af7fda4d-1111' },
    { kind: 'GoLand', dir: '/src/api', pid: 40803 },
  ],
  sessions: [
    { account: 3, label: 'c@x', provider: 'codex', id: '01a048be-2222-3333-4444-555555555555', dir: '/src/api' },
    { provider: 'claude', id: '497f1383-6666-7777-8888-999999999999', dir: '/src/web', shared: true },
  ],
  shared: { dir: '/home/me/.claude', sessions: 2648, projects: 146 },
  notes: ['no session store rota can read for kimi; their conversations are not listed'],
};

let lastPost = null, lastPath = null;
const patches = [];
const deletes = [];

// jsonAPI answers every request the page makes, recording the ones a test
// asserts on.
const jsonAPI = async (path, init = {}) => {
  const reply = doc => ({
    ok: true, status: 200, headers: { get: () => 'application/json' },
    text: async () => JSON.stringify(doc), json: async () => doc,
  });
  if (init.method === 'PATCH') {
    patches.push({ path, body: JSON.parse(init.body) });
    return reply({});
  }
  if (init.method === 'DELETE') {
    deletes.push(path);
    return reply({ removed: { id: 1 } });
  }
  if (init.method === 'POST' && path.includes('/run')) {
    lastPost = JSON.parse(init.body);
    lastPath = path;
    return reply({
      account: 1, provider: 'claude', result: 'ANSWER', is_error: false,
      exit_code: 0, duration_ms: 120, cost_usd: 0.01, session_id: 'sess-1',
    });
  }
  if (path.includes('/schema')) return reply(schema);
  if (path.includes('/accounts') && path.includes('sessions=1')) {
    return reply(Object.assign({}, accountsDoc, sessionsDoc));
  }
  if (path.includes('/accounts')) return reply(accountsDoc);
  return reply({ ok: true });
};
globalThis.fetch = jsonAPI;

const src = fs.readFileSync(T + '/pg.js', 'utf8');
const pg = new Function(src + `
;return {
  connect, render, renderPanel, renderIO, renderIONav, renderFoot, doRun, colorJSON, buildBody, runPath,
  answerSection, askSection,
  get values(){return values}, set values(v){values=v},
  get view(){return view}, set view(v){view=v},
  get ioTab(){return ioTab}, set ioTab(v){ioTab=v},
  get filter(){return filter}, set filter(v){filter=v},
  get open(){return open},
  get uploads(){return uploads},
};`)();

const findAll = (root, pred) => (root._all ? root._all().filter(pred) : []);
const byId = (root, id) => findAll(root, e => e.attrs && e.attrs.id === id)[0];
const assert = (cond, msg) => { if (!cond) { console.error('FAILED:', msg); process.exit(1); } };
// settle lets a render that fetches before it fills finish doing so.
const settle = () => new Promise(r => setTimeout(r, 5));

(async () => {
  nodes['#token'].value = 'playground-token';
  await pg.connect();
  assert(pg.view === 'ask', 'the page opens on Ask');

  // Name an account, so the form offers that provider's own catalog. The
  // rotation is exercised at step 11, where the fields are provider-neutral
  // because which provider answers is not settled until it does.
  pg.values.__account = String(accountsDoc.accounts[0].id);
  pg.render();
  const panel = nodes['#panel'];

  // 1. type a prompt into the textarea rota rendered for it
  const prompt = byId(panel, 'f_prompt');
  assert(prompt && prompt.tagName === 'TEXTAREA', 'the prompt must render as a textarea');
  prompt.value = 'summarise this repo';
  prompt.dispatch('input', { target: prompt });
  assert(pg.buildBody().prompt === 'summarise this repo', 'typing must reach the request body');

  // 2. pick a model from the account's own catalog
  const model = byId(panel, 'f_model');
  assert(model && model.tagName === 'SELECT', 'model must be a select');
  model.value = 'claude-sonnet-5';
  model.dispatch('change', { target: model });
  assert(pg.buildBody().model === 'claude-sonnet-5', 'model choice must reach the body');

  // 3. a toggle that is not streaming (streaming is exercised separately).
  // Every group's body is in the page whether it is open or not, so this
  // has to look inside Essentials rather than at the page as a whole.
  const groupNamed = name => findAll(nodes['#panel'], e => e.classList && e.classList.contains('group'))
    .find(g => g.textContent.startsWith(name));
  const essentials = groupNamed('Essentials');
  assert(essentials, 'Essentials is a group');
  const toggles = findAll(essentials, e => e.tagName === 'INPUT' && e.attrs.type === 'checkbox');
  assert(toggles.length > 0, 'Essentials has toggles');
  const notStream = toggles[toggles.length - 1];
  notStream.checked = true;
  notStream.dispatch('change', { target: notStream });
  assert(pg.buildBody().include_events === true,
    'the last Essentials toggle is include_events: ' + JSON.stringify(pg.buildBody()));

  // 4. the JSON-schema builder writes a valid schema
  const addField = findAll(essentials, e => e.tagName === 'BUTTON' && e.textContent === '+ add field')[0];
  assert(addField, 'the JSON schema builder offers to add a field');
  addField.dispatch('click', { target: addField });
  const built = pg.buildBody().json_schema;
  assert(built && built.type === 'object' && built.properties && Object.keys(built.properties).length === 1,
    'the builder must produce an object schema: ' + JSON.stringify(built));

  // 5. groups collapse by class, not by re-render, so the header a person
  // is tabbed onto survives being toggled
  const heads = findAll(panel, e => e.classList && e.classList.contains('head'));
  assert(heads.length === 5, 'every group gets a header: ' + heads.length);
  const groups = findAll(panel, e => e.classList && e.classList.contains('group'));
  const contextGroup = groups.find(g => g.textContent.startsWith('Context'));
  assert(!contextGroup.classList.contains('open'), 'Context starts collapsed');
  const context = heads.find(h => h.textContent.startsWith('Context'));
  context.dispatch('click', { target: context });
  assert(contextGroup.classList.contains('open') && context.attrs['aria-expanded'] === 'true',
    'clicking the header opens the group in place');
  assert(findAll(panel, e => e.classList && e.classList.contains('head')).includes(context),
    'the header is the same node afterwards, so focus is not thrown away');

  // 6. chips: extra directories
  const chipInput = findAll(nodes['#panel'], e => e.attrs && e.attrs.placeholder === '/srv/data, /srv/docs')[0];
  assert(chipInput, 'add_dirs renders a chips input');
  chipInput.value = '/srv/a, /srv/b';
  chipInput.dispatch('keydown', { key: 'Enter', target: chipInput });
  assert(JSON.stringify(pg.buildBody().add_dirs) === '["/srv/a","/srv/b"]',
    'chips split on commas: ' + JSON.stringify(pg.buildBody().add_dirs));

  // 7. the filter narrows across every group, opens what it matched, and
  // never destroys the box being typed into
  const filterBox = findAll(nodes['#panel'], e => e.attrs && e.attrs.type === 'search')[0];
  assert(filterBox, 'Ask offers a filter over the whole vocabulary');
  filterBox.value = 'working directory';
  filterBox.dispatch('input', { target: filterBox });
  assert(findAll(nodes['#panel'], e => e.attrs && e.attrs.type === 'search')[0] === filterBox,
    'the filter input must survive its own keystrokes');
  let open5 = findAll(nodes['#panel'], e => e.classList && e.classList.contains('group'));
  assert(open5.length >= 1 && open5.every(g => g.classList.contains('open')),
    'a filter hit must not stay hidden behind a collapsed group: ' + open5.length);
  assert(byId(nodes['#panel'], 'f_cwd') && !byId(nodes['#panel'], 'f_prompt'),
    'the filter keeps what matches and drops what does not');
  filterBox.value = 'zzz-nothing-matches';
  filterBox.dispatch('input', { target: filterBox });
  assert(nodes['#panel'].textContent.includes('Nothing matches'), 'an empty filter result says so');
  filterBox.value = '';
  filterBox.dispatch('input', { target: filterBox });
  assert(byId(nodes['#panel'], 'f_prompt'), 'clearing the filter brings everything back');

  // 8. run, and check the history it leaves behind
  const runBtn = findAll(nodes['#foot'], e => e.classList && e.classList.contains('primary'))[0];
  assert(runBtn, 'the Run button lives in the footer, below the form');
  assert(nodes['#foot'].textContent.includes('#1'), 'the footer names the account about to be spent');
  await pg.doRun();
  assert(lastPost.prompt === 'summarise this repo' && lastPost.model === 'claude-sonnet-5',
    'the run posts what the form holds');
  const hist = JSON.parse(localStorage.getItem('rota.history'));
  assert(hist && hist.length === 1 && hist[0].ok && hist[0].answer === 'ANSWER',
    'the run is remembered: ' + JSON.stringify(hist));

  // 9. the response pane shows the answer, coloured, with its metadata
  pg.ioTab = 'response'; pg.renderIO();
  const io = nodes['#io'];
  assert(io.textContent.includes('ANSWER'), 'the answer is shown');
  assert(io.textContent.includes('account 1'), 'the response says which account answered');
  const code = findAll(io, e => e.classList && e.classList.contains('code'))[0];
  assert(code && code.innerHTML.includes('j-key'), 'the response is coloured');

  // the request pane shows exactly where it goes and what it carries
  pg.ioTab = 'request'; pg.renderIO();
  assert(nodes['#io'].textContent.includes('/v1/accounts/1/run'), 'the request pane names the endpoint');
  assert(findAll(nodes['#io'], e => e.classList && e.classList.contains('code'))
    .some(c => c.innerHTML.includes('summarise this repo')), 'the request pane shows the body');

  // 10. history tab lists it
  pg.ioTab = 'history'; pg.renderIO();
  assert(nodes['#io'].textContent.includes('summarise this repo'), 'history lists the run');

  // 10b. and a run in history opens in the Request and Response panes, which
  // is the only way to see what an earlier run actually sent
  const viewBtn = findAll(nodes['#io'], e => e.tagName === 'BUTTON' && e.textContent === 'View')[0];
  assert(viewBtn, 'a history entry can be opened');
  viewBtn.dispatch('click', { target: viewBtn });
  assert(pg.ioTab === 'request', 'opening a past run shows its request');
  assert(nodes['#io'].textContent.includes('from history'), 'and says it is not the live request');
  assert(nodes['#io'].textContent.includes('/v1/accounts/1/run'), 'including the endpoint it went to');
  assert(findAll(nodes['#io'], e => e.classList && e.classList.contains('code'))
    .some(c => c.innerHTML.includes('summarise this repo')), 'the past request body is shown');
  pg.ioTab = 'response'; pg.renderIO();
  assert(nodes['#io'].textContent.includes('ANSWER'), 'the past response is shown');
  assert(nodes['#io'].textContent.includes('from history'), 'the response pane says so too');
  const back = findAll(nodes['#io'], e => e.tagName === 'BUTTON' && e.textContent.includes('live'))[0];
  assert(back, 'and there is a way back to the live request');
  back.dispatch('click', { target: back });
  assert(!nodes['#io'].textContent.includes('from history'), 'going back leaves history behind');

  // 11. the streaming path: events arrive, and the terminal event ends the run
  const sse = [
    'event: system\ndata: {"type":"system","subtype":"init","session_id":"s9"}\n\n',
    'event: assistant\ndata: {"type":"assistant","message":"hi"}\n\n',
    'event: result\ndata: {"type":"result","result":"STREAMED","is_error":false}\n\n',
    'event: done\ndata: {"type":"done","exit_code":0,"is_error":false,"session_id":"s9","duration_ms":9,"account":2}\n\n',
  ].join('');
  globalThis.fetch = async (path, init = {}) => {
    if (init.method === 'POST' && path.includes('/run')) {
      const bytes = new TextEncoder().encode(sse);
      let sent = false;
      return { ok: true, status: 200, headers: { get: () => 'text/event-stream' },
        body: { getReader: () => ({ read: async () => (sent ? { done: true } : (sent = true, { done: false, value: bytes })) }) } };
    }
    return jsonAPI(path, init);
  };
  pg.values.stream = true;
  await pg.doRun();
  pg.ioTab = 'response'; pg.renderIO();
  const streamed = nodes['#io'].textContent;
  assert(streamed.includes('STREAMED'), 'the streamed answer is shown: ' + streamed.slice(0, 200));
  assert(streamed.includes('4 events'), 'every streamed event is listed');
  const hist2 = JSON.parse(localStorage.getItem('rota.history'));
  assert(hist2.length === 2 && hist2[0].ok, 'a streamed run is remembered too');
  assert(hist2[0].account === 2, 'a streamed run records the account the terminal event named');
  pg.values.stream = false;
  globalThis.fetch = jsonAPI;

  // 12. the rotation editor: every account gets an order and a threshold,
  // and the arrows move one account a place at a time. The server shifts the
  // neighbour, so an arrow is one write that says where, not two writes of
  // numbers the page worked out.
  pg.view = 'accounts'; pg.render();
  await settle();
  const rot = nodes['#panel'];
  const boxes = findAll(rot, e => e.classList && e.classList.contains('num'));
  assert(boxes.length === accountsDoc.accounts.length * 2,
    'each account gets an order box and a threshold box: ' + boxes.length);
  assert(rot.textContent.includes('next'), 'the account a bare run would take is marked');
  const down = findAll(rot, e => e.textContent === '▼' && !('disabled' in e.attrs))[0];
  assert(down, 'the queue can be reordered from the page');
  down.dispatch('click', { target: down });
  await settle();
  assert(patches.length === 1, 'moving one account down is one write: ' + JSON.stringify(patches));
  assert(patches[0].body.order === 'down', 'and it says where: ' + JSON.stringify(patches));
  const up = findAll(nodes['#panel'], e => e.textContent === '▲' && !('disabled' in e.attrs))[0];
  assert(up, 'an account below the top can move up');
  up.dispatch('click', { target: up });
  await settle();
  assert(patches[patches.length - 1].body.order === 'up', JSON.stringify(patches));
  const orderBox = findAll(nodes['#panel'], e => e.classList && e.classList.contains('num'))[0];
  orderBox.value = '0';
  orderBox.dispatch('change', { target: orderBox });
  await settle();
  assert(patches[patches.length - 1].body.order === 0, 'typing 0 takes an account out of the rotation');
  const threshold = findAll(nodes['#panel'], e => e.classList && e.classList.contains('num'))[1];
  threshold.value = '75';
  threshold.dispatch('change', { target: threshold });
  await settle();
  assert(patches[patches.length - 1].body.threshold === 75, 'the threshold is editable too');
  const rm = findAll(nodes['#panel'], e => e.tagName === 'BUTTON' && e.textContent === 'Remove')[0];
  assert(rm, 'an account can be removed from the row it is on');
  rm.dispatch('click', { target: rm });
  await settle();
  assert(deletes.length === 1 && deletes[0].startsWith('/v1/accounts/'),
    'remove calls the right endpoint: ' + deletes);

  // 13. a run that names no account goes to the rotation's own endpoint
  pg.view = 'ask'; pg.values.__account = ''; pg.render();
  await pg.doRun();
  assert(lastPath === '/v1/run', 'leaving the account on Rotation posts to /v1/run, got ' + lastPath);
  pg.values.__account = '1';
  await pg.doRun();
  assert(lastPath === '/v1/accounts/1/run', 'naming an account still posts at it, got ' + lastPath);

  // 13b. on Rotation the form shows the union of every provider's fields.
  // A field two providers spell differently is still one field, so it must
  // appear once: rendering it twice offers a person the same choice twice
  // and posts whichever copy they touched last.
  pg.values.__account = '';
  for (const g of ['core', 'context', 'session', 'permissions', 'stream']) pg.open.add(g);
  pg.render();
  // A toggle is named by the id its label points at; every other field
  // prints its wire name in the small grey span beside the label.
  const shown = [
    ...findAll(nodes['#panel'], e => e.tagName === 'INPUT' && e.attrs.type === 'checkbox' &&
      typeof e.attrs.id === 'string' && e.attrs.id.startsWith('f_')).map(e => e.attrs.id.slice(2)),
    ...findAll(nodes['#panel'], e => e.classList && e.classList.contains('name')).map(e => e.textContent),
  ];
  const twice = shown.filter((n, i) => shown.indexOf(n) !== i);
  assert(twice.length === 0, 'the rotation form repeats: ' + twice.join(', '));
  assert(shown.includes('json_schema'), 'the rotation form offers json_schema');
  for (const gone of ['output_schema', 'fork', 'no_session_persistence']) {
    assert(!shown.includes(gone), 'the rotation form still offers ' + gone);
  }

  // 13c. the answer is shown as it was written: prose as prose, code as
  // code. rota did the splitting, so the page never parses markdown.
  const rendered = pg.answerSection({ blocks: [
    { kind: 'text', text: 'here:' },
    { kind: 'code', lang: 'go', text: 'x := 1' },
  ] });
  assert(rendered, 'an answer with blocks is shown');
  assert(findAll(rendered, e => e.tagName === 'PRE').length === 1, 'code is a pre');
  assert(findAll(rendered, e => e.tagName === 'P' && e.textContent === 'here:').length === 1, 'prose is a paragraph');
  assert(!pg.answerSection({}), 'an answer with no blocks shows nothing');

  // 13d. a run that ended by asking shows the question, and a task list is
  // offered as checkboxes rather than as one choice of several.
  const asked = pg.askSection({ ask: { kind: 'choice', question: 'Which?', options: ['a', 'b'], multiple: true } });
  assert(asked, 'a question is shown');
  const askBoxes = findAll(asked, e => e.tagName === 'INPUT');
  assert(askBoxes.length === 2, 'one control per option: ' + askBoxes.length);
  assert(askBoxes.every(b => b.attrs.type === 'checkbox'), 'a task list means more than one may be taken');
  assert(askBoxes.every(b => 'disabled' in b.attrs), 'a headless run has already exited: nothing to click');
  const single = pg.askSection({ ask: { kind: 'choice', question: 'Which?', options: ['a', 'b'] } });
  assert(findAll(single, e => e.tagName === 'INPUT').every(b => b.attrs.type === 'radio'),
    'a plain list is one choice');
  assert(!pg.askSection({}), 'an answer that asked nothing shows nothing');

  // 13e. every account can be told where it belongs
  pg.view = 'accounts'; pg.render();
  await settle();
  const paths = findAll(nodes['#panel'], e => e.classList && e.classList.contains('pathin'));
  assert(paths.length === accountsDoc.accounts.length * 2,
    'a working directory and a config directory each: ' + paths.length);
  pg.view = 'ask'; pg.render();

  // 15. Running shows what is open and what could be resumed, with the two
  // things that have no owner said rather than hidden: an instance nothing can
  // attribute, and the pool of conversations every account shares.
  pg.view = 'running'; pg.render();
  await settle();
  const running = nodes['#panel'].textContent;
  assert(running.includes('Running now'), 'the Running view lists what is open: ' + running);
  assert(running.includes('#2 a@x'), 'an instance rota started names the account paying: ' + running);
  assert(running.includes('GoLand'), 'and an editor is named by its own name: ' + running);
  assert(running.includes('af7fda4d'), 'a running instance shows the conversation it is in');
  assert(running.includes('Could be resumed'), 'and what could be picked up again');
  assert(running.includes('#3 c@x'), 'a conversation in an account of its own is attributed');
  assert(running.includes('shared'), 'one in the shared home says so rather than claiming an account');
  assert(running.includes('2648'), 'the shared pool is counted, not listed');
  assert(running.includes('kimi'), 'and a provider rota cannot read is said, not silently empty');

  // The whole id must be reachable: --resume takes all of it, and the table
  // shows eight characters.
  const copy = findAll(nodes['#panel'], e => e.attrs && e.attrs.title &&
    String(e.attrs.title).startsWith('01a048be'))[0];
  assert(copy, 'a session offers its whole id, not the prefix on screen');
  assert(String(copy.attrs.title).length > 30, 'and the whole of it: ' + copy.attrs.title);

  pg.view = 'ask'; pg.render();

  // every view, account and group must render without throwing
  let passes = 0;
  for (const v of ['ask', 'accounts', 'running', 'signin', 'console']) { pg.view = v; pg.render(); passes++; }
  // 16. a hidden provider is not offered for sign-in, and the rest are
  pg.view = 'signin'; pg.render();
  const offered = findAll(nodes['#panel'], e => e.tagName === 'OPTION').map(o => o.attrs.value);
  assert(offered.includes('claude') && !offered.includes('kimi'), 'sign-in offers the visible providers only: ' + offered.join(','));
  pg.view = 'ask';
  for (const acct of accountsDoc.accounts) {
    pg.values.__account = String(acct.id);
    for (const g of ['core', 'context', 'session', 'permissions', 'stream']) {
      pg.open.add(g); pg.renderPanel(); passes++;
    }
  }
  for (const io of ['request', 'response', 'history']) { pg.ioTab = io; pg.renderIO(); passes++; }
  assert(passes >= 20, 'every panel renders');

  // the colouring must actually mark every kind of JSON token
  const coloured = pg.colorJSON({ a: 1, b: "x", c: true, d: null });
  for (const cls of ['j-key', 'j-num', 'j-str', 'j-bool', 'j-null']) {
    assert(coloured.includes(cls), 'JSON colouring marks ' + cls);
  }

  console.log('PLAYGROUND_OK passes=' + passes);
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
