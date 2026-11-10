// How a session is said: words.js, one assertion for each of its rules.
const kit = require(process.env.T + '/term/kit.js');
const { assert } = kit;

(async () => {
  const page = await kit.load({ signedIn: false });
  const tp = page.tp;
  const now = kit.CLOCK;
  tp.termNow = () => now;
  const same = (got, want, what) =>
    assert(JSON.stringify(got) === JSON.stringify(want), what + ': ' + JSON.stringify(got));

  // termName: a name, then a label, then the id.
  assert(tp.termName({ id: 'a1', label: 'lab', name: 'nm' }) === 'nm', 'a name comes first');
  assert(tp.termName({ id: 'a1', label: 'lab' }) === 'lab', 'then the label');
  assert(tp.termName({ id: 'a1', label: '', name: '' }) === 'a1', 'then the id');

  // termStatusOf: a stopped session is stopped or ended, faint; the rest by
  // their status.
  same(tp.termStatusOf({ ended: true, held: true }), { key: 'stopped', word: 'stopped', tone: 'faint' },
    'a held session that has ended has stopped');
  same(tp.termStatusOf({ ended: true }), { key: 'stopped', word: 'ended', tone: 'faint' },
    'a terminal that is not held has ended');
  same(tp.termStatusOf({ status: 'stopped', held: true }), { key: 'stopped', word: 'stopped', tone: 'faint' },
    'a status of stopped is stopped too');
  same(tp.termStatusOf({}), { key: 'running', word: 'running', tone: 'ok' }, 'no status is running');
  same(tp.termStatusOf({ status: 'running' }), { key: 'running', word: 'running', tone: 'ok' }, 'running is ok');
  same(tp.termStatusOf({ status: 'needs_you' }), { key: 'needs_you', word: 'needs you', tone: 'warn' },
    'needs_you is a warning');
  same(tp.termStatusOf({ status: 'quiet' }), { key: 'quiet', word: 'quiet', tone: 'muted' }, 'quiet is muted');

  // termEnding: how a session ended.
  assert(tp.termEnding({ lost: true, ending: 'its sharer stopped' }) === 'its sharer stopped',
    'a lost shared terminal says what the server said');
  assert(tp.termEnding({ lost: true }) === 'the sharing process went away', 'or that its sharer went away');
  assert(tp.termEnding({ state: 'interrupted', exit: 1 }) === 'interrupted', 'an interrupted one says so');
  assert(tp.termEnding({ state: 'broken' }) === 'its record cannot be read', 'a broken one says its record is unreadable');
  assert(tp.termEnding({ ended: true, exit: 2 }) === 'ended, code 2', 'anything else gives its code');

  // termCredential: the session's own word first, then its account's.
  const accounts = [
    { id: 1, credential: 'apikey' },
    { id: 2, long_until: new Date(now + 86400000).toISOString() },
    { id: 3, long_until: new Date(now - 1000).toISOString() },
    { id: 4 },
  ];
  assert(tp.termCredential({ kind: 'shell' }, accounts) === '', 'a shell has no credential');
  assert(tp.termCredential({ account: { id: 1 } }, accounts) === 'API key', 'an apikey account is an API key');
  assert(tp.termCredential({ account: { id: 2 } }, accounts) === 'long token', 'a long token still good is a long token');
  assert(tp.termCredential({ account: { id: 3 } }, accounts) === 'login', 'one that has lapsed is a login');
  assert(tp.termCredential({ account: { id: 4 } }, accounts) === 'login', 'a plain account is a login');
  assert(tp.termCredential({ account: { id: 9 } }, accounts) === 'login', 'an account not listed is a login');
  assert(tp.termCredential({ account: { id: 1, credential: 'long' } }, accounts) === 'long token',
    "the session's own credential wins over its account's");
  assert(tp.termCredential({ account: { id: 2, credential: 'login' } }, accounts) === 'login', 'login is login');
  assert(tp.termCredential({ account: { id: 4, credential: 'apikey' } }, accounts) === 'API key', 'apikey is API key');

  // termWaitingWord: what a session is waiting on, in words.
  assert(tp.termWaitingWord('permission') === 'a permission', 'a permission');
  assert(tp.termWaitingWord('question') === 'a question', 'a question');
  assert(tp.termWaitingWord('') === '', 'nothing is nothing');
  assert(tp.termWaitingWord('a plan') === 'a plan', 'anything else as it is');

  // termBase: the last element of a path.
  assert(tp.termBase('/src/api') === 'api', 'the last element');
  assert(tp.termBase('/src/api/') === 'api', 'a trailing slash is not an element');
  assert(tp.termBase('C:\\src\\web') === 'web', 'a Windows path too');
  assert(tp.termBase('') === '', 'no path is nothing');

  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
