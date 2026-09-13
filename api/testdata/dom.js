// A minimal DOM, enough to run the playground's rendering code outside a
// browser and see whether it throws.
class CL {
  constructor(n){ this.n=n; this.s=new Set(); }
  add(...c){ c.forEach(x=>x&&this.s.add(x)); }
  remove(...c){ c.forEach(x=>this.s.delete(x)); }
  toggle(c,on){ on?this.s.add(c):this.s.delete(c); }
  contains(c){ return this.s.has(c); }
}
class El {
  constructor(tag){ this.tagName=(tag||'').toUpperCase(); this.children=[]; this.attrs={};
    this.listeners={}; this.classList=new CL(this); this.style={}; this.dataset={};
    this._text=''; this.value=''; this.checked=false; this.files=[]; this.selectionStart=0; this.selectionEnd=0; }
  set textContent(v){ this._text=String(v); this.children=[]; }
  get textContent(){ return this._text + this.children.map(c=>c.textContent||'').join(''); }
  set innerHTML(v){ this._html=String(v); }
  get innerHTML(){ return this._html||''; }
  set className(v){ this.attrs.class=v; this.classList=new CL(this); String(v).split(/\s+/).forEach(c=>c&&this.classList.add(c)); }
  get className(){ return this.attrs.class||''; }
  setAttribute(k,v){ this.attrs[k]=String(v); if(k==='class') this.className=v;
    if(k==='value') this.value=v; if(k==='checked') this.checked=true;
    if(k.startsWith('data-')) this.dataset[k.slice(5).replace(/-(\w)/g,(_,c)=>c.toUpperCase())]=String(v); }
  getAttribute(k){ return this.attrs[k]; }
  removeAttribute(k){ delete this.attrs[k]; }
  append(...kids){ for(const k of kids){ if(k===null||k===undefined) continue;
    this.children.push(typeof k==='object'?k:{textContent:String(k),children:[],attrs:{}}); } }
  replaceChildren(...kids){ this.children=[]; this._text=''; this.append(...kids); }
  addEventListener(t,f){ (this.listeners[t]=this.listeners[t]||[]).push(f); }
  removeEventListener(){}
  dispatch(t,ev={}){ for(const f of this.listeners[t]||[]) f(Object.assign({preventDefault(){},stopPropagation(){},target:this},ev)); }
  remove(){}
  focus(){}
  setRangeText(){}
  _all(){ return this.children.flatMap(c=>c._all?[c,...c._all()]:[c]); }
  querySelector(sel){ return this.querySelectorAll(sel)[0]||null; }
  querySelectorAll(sel){
    const m = el => {
      if(sel.startsWith('.')) return el.classList&&el.classList.contains(sel.slice(1));
      if(sel.startsWith('#')) return el.attrs&&el.attrs.id===sel.slice(1);
      if(sel.startsWith('[')){ const k=sel.slice(1,-1).split('=')[0]; return el.attrs&&(k in el.attrs); }
      const parts=sel.split(' ');
      return el.tagName===parts[parts.length-1].toUpperCase();
    };
    return this._all().filter(m);
  }
}
const nodes = {};
const doc = {
  createElement: t => new El(t),
  createTextNode: t => ({ textContent: String(t), children: [], attrs: {} }),
  querySelector: sel => nodes[sel] || (nodes[sel] = new El('div')),
  addEventListener: () => {},
  body: new El('body'),
  documentElement: new El('html'),
};
globalThis.document = doc;
globalThis.window = { open: () => {} };
globalThis.alert = () => {};
globalThis.confirm = () => true;
globalThis.navigator = { clipboard: { writeText: () => {} } };
const mem = () => { const m = {}; return { getItem: k => m[k] ?? null, setItem: (k, v) => m[k] = String(v), removeItem: k => delete m[k] }; };
globalThis.sessionStorage = mem();
globalThis.localStorage = mem();
globalThis.FileReader = class { readAsDataURL() { this.result = "data:,AQI="; this.onload && this.onload(); } };
globalThis.location = { origin: 'http://127.0.0.1:8787' };

// A WebSocket the test drives by hand: it records the URL and the protocols
// the page asked for and everything the page sent, and open/feed/close are how
// a test plays the server's side of it.
const sockets = [];
class WS {
  constructor(url, protocols) { this.url = url; this.protocols = protocols; this.sent = []; sockets.push(this); }
  send(data) { this.sent.push(data); }
  open() { this.onopen && this.onopen({}); }
  feed(doc) { this.onmessage && this.onmessage({ data: typeof doc === 'string' ? doc : JSON.stringify(doc) }); }
  close() { this.onclose && this.onclose({}); }
}
globalThis.WebSocket = WS;
module.exports = { doc, nodes, El, sockets };
