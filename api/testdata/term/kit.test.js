// The kit itself: every way a page could have a string parsed as markup is
// caught, the header page.js draws is not, and a page that has written
// markup cannot pass.
const kit = require(process.env.T + '/term/kit.js');
const { assert } = kit;

(async () => {
  const page = await kit.load({ signedIn: true });
  const tp = page.tp;
  assert(page.markup.length === 0, 'signing in draws only the header: ' + JSON.stringify(page.markup));
  assert(page.nodes['#top'].innerHTML === tp.HEADER, 'and the header is drawn');

  const span = () => tp.el('span');
  const tries = {
    'an html attribute': () => tp.el('b', { html: '<img src=x onerror=alert(1)>' }),
    'a server object as attributes': () => tp.el('span', JSON.parse('{"html":"<b>x</b>"}')),
    'innerHTML by a computed name': () => { span()['inner' + 'HTML'] = '<b>x</b>'; },
    'innerHTML on the header with something else': () => { page.nodes['#top'].innerHTML = '<b>x</b>'; },
    outerHTML: () => { span().outerHTML = '<b>x</b>'; },
    insertAdjacentHTML: () => span().insertAdjacentHTML('beforeend', '<b>x</b>'),
    setHTMLUnsafe: () => span().setHTMLUnsafe('<b>x</b>'),
    DOMParser: () => new DOMParser().parseFromString('<b>x</b>', 'text/html'),
    createContextualFragment: () => document.createRange().createContextualFragment('<b>x</b>'),
    'document.write': () => document.write('<b>x</b>'),
  };
  for (const [what, write] of Object.entries(tries)) {
    const before = page.markup.length;
    let threw = false;
    try { write(); } catch { threw = true; }
    assert(threw, what + ' is refused');
    assert(page.markup.length === before + 1, what + ' is recorded, even if a page swallowed the throw');
  }
  // text is text
  const b = tp.el('b', { text: '<b>x</b>' });
  assert(b.textContent === '<b>x</b>' && page.markup.length === Object.keys(tries).length,
    'text goes in as text and is not markup');

  page.markup.length = 0;
  kit.ok();
})().catch(e => { console.error('FAILED:', e.stack); process.exit(1); });
