'use strict';

// Harness for the SPA runtime's DOM patcher and component lifecycle.
//
// The existing spa_runtime_harness.js drives the parts of the runtime that are
// observable through fetches and watcher calls, using stub elements. Patching
// cannot be tested that way: the whole point of it is which DOM nodes survive
// a render, so the assertions are about node identity, focus, cursor position,
// scroll offset and class lists — none of which a stub can represent.
//
// So this file implements a small real DOM: an HTML parser and Element/Text
// nodes with the subset of the standard API the runtime actually touches. It
// is deliberately a SUBSET, not a browser:
//
//   - Selectors: tag, #id, .class, [attr], [attr="value"], and compounds of
//     those (input.foo[data-key="1"]). No combinators, no pseudo-classes.
//   - The parser handles tags, quoted/unquoted attributes, text, comments,
//     void elements and <template>. No namespaces, no implied tags (a bare
//     <tr> is not relocated into a <tbody> the way a browser would), no
//     character-entity decoding beyond the few below.
//   - Layout does not exist: scrollTop and selectionStart are plain
//     properties that the patcher must simply not disturb.
//
// That subset is enough to prove the properties under test, and every
// assertion below fails loudly rather than silently passing if the DOM here
// does not support what the runtime asks of it.
//
// Usage: node spa_patch_harness.js <path-to-extracted-runtime.js>

const fs = require('fs');
const vm = require('vm');

const runtimeSrc = fs.readFileSync(process.argv[2], 'utf8');

const failures = [];
function check(name, cond, detail) {
  if (!cond) failures.push(name + (detail ? ': ' + detail : ''));
}
function eq(name, got, want) {
  check(name, Object.is(got, want), 'got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}

// ── Nodes ───────────────────────────────────────────────────────────────────

const VOID = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input',
  'link', 'meta', 'param', 'source', 'track', 'wbr']);

let uid = 0;

class Text {
  constructor(data) {
    this.nodeType = 3;
    this.data = data;
    this.parentNode = null;
    this._uid = ++uid;
  }
  get textContent() { return this.data; }
  set textContent(v) { this.data = String(v); }
  get childNodes() { return []; }
  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
}

class Comment extends Text {
  constructor(data) { super(data); this.nodeType = 8; }
}

class Element {
  constructor(tag) {
    this.nodeType = 1;
    this.tagName = String(tag).toUpperCase();
    this.childNodes = [];
    this.parentNode = null;
    this._attrs = new Map();
    this._uid = ++uid;

    // Properties the patcher must preserve. In a browser these are owned by
    // layout and by the text-editing machinery; here they are just state that
    // an innerHTML rebuild would lose along with the element itself.
    this.scrollTop = 0;
    this.selectionStart = null;
    this.value = '';
  }

  get id() { return this.getAttribute('id') || ''; }

  get attributes() {
    const out = [];
    for (const [name, value] of this._attrs) out.push({ name, value });
    return out;
  }

  getAttribute(n) { return this._attrs.has(n) ? this._attrs.get(n) : null; }
  setAttribute(n, v) { this._attrs.set(n, String(v)); }
  hasAttribute(n) { return this._attrs.has(n); }
  removeAttribute(n) { this._attrs.delete(n); }

  get classList() {
    const el = this;
    const read = () => (el.getAttribute('class') || '').split(/\s+/).filter(Boolean);
    const write = (l) => el.setAttribute('class', l.join(' '));
    return {
      add(c) { const l = read(); if (l.indexOf(c) === -1) { l.push(c); write(l); } },
      remove(c) { write(read().filter((x) => x !== c)); },
      contains(c) { return read().indexOf(c) !== -1; },
    };
  }

  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
  get firstChild() { return this.childNodes[0] || null; }
  get children() { return this.childNodes.filter((n) => n.nodeType === 1); }

  appendChild(node) { return this.insertBefore(node, null); }

  insertBefore(node, ref) {
    if (node.parentNode) node.parentNode.removeChild(node);
    // A DocumentFragment inserts its children, not itself.
    if (node.nodeType === 11) {
      const kids = node.childNodes.slice();
      for (const k of kids) this.insertBefore(k, ref);
      return node;
    }
    const i = ref ? this.childNodes.indexOf(ref) : -1;
    if (i === -1) this.childNodes.push(node); else this.childNodes.splice(i, 0, node);
    node.parentNode = this;
    return node;
  }

  removeChild(node) {
    const i = this.childNodes.indexOf(node);
    if (i !== -1) this.childNodes.splice(i, 1);
    node.parentNode = null;
    return node;
  }

  replaceChild(fresh, old) {
    this.insertBefore(fresh, old);
    return this.removeChild(old);
  }

  get textContent() { return this.childNodes.map((n) => n.textContent).join(''); }
  set textContent(v) {
    for (const k of this.childNodes.slice()) this.removeChild(k);
    if (v !== '') this.appendChild(new Text(String(v)));
  }

  get innerHTML() { return this.childNodes.map(serialize).join(''); }
  set innerHTML(html) {
    for (const k of this.childNodes.slice()) this.removeChild(k);
    for (const n of parseFragment(String(html))) this.appendChild(n);
  }

  matches(sel) { return matchesSelector(this, sel); }

  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => {
      for (const k of n.childNodes) {
        if (k.nodeType !== 1) continue;
        if (matchesSelector(k, sel)) out.push(k);
        walk(k);
      }
    };
    walk(this);
    // The runtime calls .forEach on the result of querySelectorAll, which a
    // real NodeList supports; a plain Array does too.
    return out;
  }

  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }

  closest(sel) {
    let n = this;
    while (n && n.nodeType === 1) {
      if (matchesSelector(n, sel)) return n;
      n = n.parentNode;
    }
    return null;
  }

  focus() { doc.activeElement = this; }
}

class Fragment extends Element {
  constructor() { super('#fragment'); this.nodeType = 11; }
}

class TemplateElement extends Element {
  constructor() {
    super('template');
    this.content = new Fragment();
  }
  set innerHTML(html) {
    for (const k of this.content.childNodes.slice()) this.content.removeChild(k);
    for (const n of parseFragment(String(html))) this.content.appendChild(n);
  }
  get innerHTML() { return this.content.childNodes.map(serialize).join(''); }
}

function serialize(node) {
  if (node.nodeType === 3) return node.data;
  if (node.nodeType === 8) return '<!--' + node.data + '-->';
  const tag = node.tagName.toLowerCase();
  let s = '<' + tag;
  for (const { name, value } of node.attributes) s += ' ' + name + '="' + value + '"';
  s += '>';
  if (VOID.has(tag)) return s;
  return s + node.childNodes.map(serialize).join('') + '</' + tag + '>';
}

// ── Parser ──────────────────────────────────────────────────────────────────

function decode(s) {
  return s.replace(/&lt;/g, '<').replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');
}

function parseFragment(html) {
  const root = new Fragment();
  let stack = [root];
  let i = 0;

  const top = () => stack[stack.length - 1];

  while (i < html.length) {
    const lt = html.indexOf('<', i);
    if (lt === -1) {
      if (i < html.length) top().appendChild(new Text(decode(html.slice(i))));
      break;
    }
    if (lt > i) top().appendChild(new Text(decode(html.slice(i, lt))));

    if (html.startsWith('<!--', lt)) {
      const end = html.indexOf('-->', lt + 4);
      const stop = end === -1 ? html.length : end;
      top().appendChild(new Comment(html.slice(lt + 4, stop)));
      i = end === -1 ? html.length : end + 3;
      continue;
    }

    const gt = html.indexOf('>', lt);
    if (gt === -1) { top().appendChild(new Text(decode(html.slice(lt)))); break; }
    let raw = html.slice(lt + 1, gt).trim();
    i = gt + 1;

    if (raw.startsWith('/')) {
      const name = raw.slice(1).trim().toLowerCase();
      for (let d = stack.length - 1; d > 0; d--) {
        if (stack[d].tagName.toLowerCase() === name) { stack = stack.slice(0, d); break; }
      }
      continue;
    }

    const selfClose = raw.endsWith('/');
    if (selfClose) raw = raw.slice(0, -1).trim();

    const m = /^([a-zA-Z0-9-]+)\s*([\s\S]*)$/.exec(raw);
    if (!m) continue;
    const tag = m[1].toLowerCase();
    const el = tag === 'template' ? new TemplateElement() : new Element(tag);

    const attrRe = /([a-zA-Z_:@][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?/g;
    let a;
    while ((a = attrRe.exec(m[2])) !== null) {
      const v = a[2] !== undefined ? a[2] : a[3] !== undefined ? a[3] : a[4] !== undefined ? a[4] : '';
      el.setAttribute(a[1], decode(v));
    }

    top().appendChild(el);

    // <script> content is raw text up to its closing tag, not markup.
    if (tag === 'script' || tag === 'style') {
      const close = html.toLowerCase().indexOf('</' + tag, i);
      const stop = close === -1 ? html.length : close;
      const body = html.slice(i, stop);
      if (body) el.appendChild(new Text(body));
      i = close === -1 ? html.length : html.indexOf('>', close) + 1;
      continue;
    }

    if (!selfClose && !VOID.has(tag)) stack.push(el);
  }

  return root.childNodes.slice();
}

// ── Selectors ───────────────────────────────────────────────────────────────

function matchesSelector(el, sel) {
  if (el.nodeType !== 1) return false;
  const parts = String(sel).split(',').map((s) => s.trim()).filter(Boolean);
  return parts.some((one) => matchesOne(el, one));
}

function matchesOne(el, sel) {
  const re = /([#.]?[a-zA-Z0-9_-]+)|(\[[^\]]+\])/g;
  let m, ok = true, saw = false;
  while ((m = re.exec(sel)) !== null) {
    saw = true;
    const tok = m[0];
    if (tok.startsWith('#')) {
      if (el.getAttribute('id') !== tok.slice(1)) ok = false;
    } else if (tok.startsWith('.')) {
      if (!el.classList.contains(tok.slice(1))) ok = false;
    } else if (tok.startsWith('[')) {
      const inner = tok.slice(1, -1);
      const eqAt = inner.indexOf('=');
      if (eqAt === -1) {
        if (!el.hasAttribute(inner.trim())) ok = false;
      } else {
        const name = inner.slice(0, eqAt).trim();
        const want = inner.slice(eqAt + 1).trim().replace(/^["']|["']$/g, '');
        if (el.getAttribute(name) !== want) ok = false;
      }
    } else if (tok !== '*') {
      if (el.tagName !== tok.toUpperCase()) ok = false;
    }
  }
  return saw && ok;
}

// ── Document / window ───────────────────────────────────────────────────────

const doc = {
  nodeType: 9,
  activeElement: null,
  createElement(tag) { return String(tag).toLowerCase() === 'template' ? new TemplateElement() : new Element(tag); },
  createTextNode(t) { return new Text(t); },
  getElementById(id) { return doc.body ? (doc.body.querySelector('#' + id) || (doc.body.id === id ? doc.body : null)) : null; },
  querySelector(sel) { return doc.body ? doc.body.querySelector(sel) : null; },
  querySelectorAll(sel) { return doc.body ? doc.body.querySelectorAll(sel) : []; },
  addEventListener() {},
  removeEventListener() {},
};
doc.body = new Element('body');
doc.body.setAttribute('id', 'body');

const location = { origin: 'https://example.test', protocol: 'https:', host: 'example.test', pathname: '/', search: '', hash: '', href: 'https://example.test/' };
const history = { state: null, scrollRestoration: 'auto', replaceState() {}, pushState() {} };

const window = {
  location, history, scrollY: 0, scrollTo() {},
  addEventListener() {}, removeEventListener() {}, dispatchEvent() { return true; },
};

const sandbox = {
  window, document: doc, history, location,
  fetch: () => Promise.resolve({ ok: true, status: 200, headers: { get: () => null }, text: () => Promise.resolve('') }),
  CustomEvent: class { constructor(t, i) { this.type = t; this.detail = (i || {}).detail; } },
  AbortController: class { constructor() { this.signal = {}; } abort() {} },
  HTMLFormElement: function HTMLFormElement() {},
  WebSocket: function WebSocket() {},
  console,
  setTimeout, clearTimeout, setInterval, clearInterval,
  requestAnimationFrame: (fn) => setTimeout(fn, 0),
  URL, URLSearchParams, FormData: function FormData() {},
  Promise, Map, Set, WeakMap, Proxy, Reflect, Object, Array, JSON, String, Number, Math, isNaN, RegExp, Error,
};
sandbox.globalThis = sandbox;
sandbox.HTMLFormElement.prototype.submit = function () {};

vm.createContext(sandbox);
vm.runInContext(runtimeSrc, sandbox, { filename: 'breeze-runtime.js' });

const breeze = sandbox.window.breeze;
const Breeze = sandbox.window.Breeze;
check('runtime exposes window.breeze', !!breeze);
check('runtime exposes Breeze.onMount', typeof (Breeze && Breeze.onMount) === 'function');
check('runtime exposes Breeze.onDestroy', typeof (Breeze && Breeze.onDestroy) === 'function');

// A guard on the harness itself: if the mini-DOM could not parse and rebuild
// markup, every assertion below would pass for the wrong reason.
(function selfTest() {
  const probe = new Element('div');
  probe.innerHTML = '<p class="a" data-key="1">hi<span>x</span></p><input value="v">';
  eq('self-test: parsed child count', probe.childNodes.length, 2);
  eq('self-test: attribute read', probe.firstChild.getAttribute('data-key'), '1');
  eq('self-test: text content', probe.firstChild.textContent, 'hix');
  eq('self-test: selector match', probe.querySelectorAll('p.a[data-key="1"]').length, 1);
  eq('self-test: void element', probe.childNodes[1].tagName, 'INPUT');
})();

const app = new Element('div');
app.setAttribute('id', 'breeze-app');
doc.body.appendChild(app);

function main() {
  // 1. An element that survives a render is the same node, not a replacement.
  breeze.swap(app, '<ul><li>one</li></ul>');
  const ul = app.querySelector('ul');
  const li = app.querySelector('li');
  breeze.swap(app, '<ul><li>two</li></ul>');
  check('surviving parent keeps node identity', app.querySelector('ul') === ul);
  check('surviving child keeps node identity', app.querySelector('li') === li);
  eq('changed text is updated in place', li.textContent, 'two');

  // 2. Focus and cursor position survive a render. This is the concrete bug:
  // typing in a search box while a poll refreshed the region used to drop the
  // caret to the start of an empty field on every tick.
  breeze.swap(app, '<form><input id="q" value="hel"><span>0 results</span></form>');
  const input = app.querySelector('#q');
  input.focus();
  input.value = 'hello';
  input.selectionStart = 5;
  breeze.swap(app, '<form><input id="q" value="hel"><span>7 results</span></form>');
  check('focused element is not replaced', app.querySelector('#q') === input);
  check('focus is retained across a render', doc.activeElement === input);
  eq('cursor position is retained', input.selectionStart, 5);
  eq('sibling text still updates', app.querySelector('span').textContent, '7 results');

  // 3. Scroll offset inside the region survives.
  breeze.swap(app, '<div id="log">a</div>');
  const log = app.querySelector('#log');
  log.scrollTop = 250;
  breeze.swap(app, '<div id="log">b</div>');
  check('scrolled element is not replaced', app.querySelector('#log') === log);
  eq('scroll offset is retained', log.scrollTop, 250);

  // 4. A class added by JS to an element the render did not change is left
  // alone, so an "active"/"open" state set client-side is not wiped by an
  // unrelated update elsewhere in the region.
  breeze.swap(app, '<div><b id="keep">k</b><i id="chg">old</i></div>');
  const keep = app.querySelector('#keep');
  keep.classList.add('active');
  breeze.swap(app, '<div><b id="keep">k</b><i id="chg">new</i></div>');
  check('JS-added class on an untouched sibling survives', keep.classList.contains('active'));
  eq('the sibling that did change was updated', app.querySelector('#chg').textContent, 'new');

  // 5. Attributes are synced both ways, without gratuitous rewrites.
  breeze.swap(app, '<a id="lnk" href="/a" title="t">x</a>');
  const lnk = app.querySelector('#lnk');
  breeze.swap(app, '<a id="lnk" href="/b" data-new="1">x</a>');
  check('attribute element kept identity', app.querySelector('#lnk') === lnk);
  eq('changed attribute is updated', lnk.getAttribute('href'), '/b');
  eq('new attribute is added', lnk.getAttribute('data-new'), '1');
  eq('dropped attribute is removed', lnk.getAttribute('title'), null);

  // 6. data-key drives matching, so a reorder moves nodes instead of
  // rewriting their contents.
  breeze.swap(app, '<ul><li data-key="a">A</li><li data-key="b">B</li></ul>');
  const a = app.querySelector('[data-key="a"]');
  const b = app.querySelector('[data-key="b"]');
  breeze.swap(app, '<ul><li data-key="b">B</li><li data-key="a">A</li></ul>');
  const order = app.querySelectorAll('li');
  check('keyed reorder moves the same nodes', order[0] === b && order[1] === a);

  // 7. data-spa-static marks a subtree the runtime must not touch, for DOM
  // owned by something else (a chart library, an editor widget).
  breeze.swap(app, '<div data-spa-static><canvas></canvas></div>');
  const holder = app.querySelector('[data-spa-static]');
  holder.appendChild(doc.createElement('span')); // stand-in for widget-built DOM
  breeze.swap(app, '<div data-spa-static></div>');
  check('static subtree is left alone', holder.querySelectorAll('span').length === 1);

  // 8. onMount fires once on insert, and NOT when the element merely updates.
  let mounts = 0, destroys = 0;
  const un = Breeze.onMount('.widget', function (el) {
    mounts++;
    return function () { destroys++; };
  });
  breeze.swap(app, '<div class="widget"><span>1</span></div>');
  eq('onMount fires on insert', mounts, 1);
  breeze.swap(app, '<div class="widget"><span>2</span></div>');
  eq('onMount does not re-fire on a content update', mounts, 1);
  eq('onDestroy has not fired while the element lives', destroys, 0);

  // 9. onDestroy fires when the element is actually removed.
  breeze.swap(app, '<p>gone</p>');
  eq('onDestroy fires on removal', destroys, 1);
  eq('onMount was not re-entered by the removal', mounts, 1);

  // 10. Re-inserting a fresh matching element mounts again.
  breeze.swap(app, '<div class="widget">again</div>');
  eq('a new element mounts again', mounts, 2);
  un();
  breeze.swap(app, '<p>x</p>');
  eq('unregistering stops later mounts', mounts, 2);

  // 11. The declarative form works the same way.
  let attrMounts = 0;
  sandbox.window.initThing = function () { attrMounts++; };
  breeze.swap(app, '<canvas data-spa-mount="initThing"></canvas>');
  eq('data-spa-mount fires once', attrMounts, 1);
  breeze.swap(app, '<canvas data-spa-mount="initThing" data-x="2"></canvas>');
  eq('data-spa-mount does not re-fire on update', attrMounts, 1);

  // 12. The opt-out restores the old wholesale replacement.
  breeze.swap(app, '<div id="old">x</div>');
  const old = app.querySelector('#old');
  sandbox.window.__BREEZE_NO_PATCH__ = true;
  breeze.swap(app, '<div id="old">y</div>');
  check('opt-out replaces nodes as before', app.querySelector('#old') !== old);
  sandbox.window.__BREEZE_NO_PATCH__ = false;
}

// ── Reactivity without navigation ───────────────────────────────────────────
//
// The bug these cover: "data only updates on tab change, not reactively."
//
// Store mutations always ran the whole path — proxy trap, watcher, template
// render, swap, patch — so nothing looked broken from the inside. What was
// broken is WHERE the render landed: breeze.bind() remembered the element it
// resolved at registration time, and the patcher is allowed to replace a node
// (see _sameType) or drop it as leftover. Once that happened the binding
// patched a node that was no longer in the document. Only a navigation, which
// re-runs the view's inline script and so re-registers the binding against
// the live node, appeared to "fix" it — hence the tab-change symptom.
//
// Every assertion below therefore performs TWO consecutive updates with no
// navigation between them, and asserts the SECOND one is on screen.

function tick() { return new Promise((r) => setTimeout(r, 0)); }

// Templates and data are normally embedded by the server as JSON tags; the
// runtime reads them from the document, so the harness provides them the same
// way rather than reaching inside the runtime.
function setTag(id, obj) {
  let el = doc.getElementById(id);
  if (!el) {
    el = new Element('script');
    el.setAttribute('id', id);
    doc.body.appendChild(el);
  }
  el.textContent = JSON.stringify(obj);
  return el;
}

async function reactivity() {
  setTag('__breeze_tmpl__', {
    counter: '<b>{{.count}}</b>',
    boxed:   '<b>{{.count}}</b>',
  });
  setTag('__breeze_data__', { count: 1 });

  // 13. Two consecutive mutations on the same route both reach the DOM.
  breeze.swap(app, '<div id="live"></div>');
  const live = app.querySelector('#live');
  const unbind = breeze.bind(live, 'counter');
  eq('bind renders immediately', live.textContent, '1');

  breeze.data().count = 2;
  await tick();
  eq('first mutation renders without navigation', live.textContent, '2');

  breeze.data().count = 3;
  await tick();
  eq('second consecutive mutation renders too', live.textContent, '3');

  // 14. The region's node is replaced by an unrelated render (DIV -> SPAN is
  // a type change, so the patcher replaces rather than reuses). The binding
  // must follow the replacement instead of patching the detached original.
  breeze.swap(app, '<span id="live"></span>');
  const replaced = app.querySelector('#live');
  check('the region really was replaced', replaced !== live);

  breeze.data().count = 4;
  await tick();
  eq('update follows a replaced region', replaced.textContent, '4');
  eq('the detached node is not what updated', live.textContent, '3');

  breeze.data().count = 5;
  await tick();
  eq('and keeps following it on the next update', replaced.textContent, '5');
  unbind();

  // 15. A region with neither id nor data-key is still recoverable, via the
  // stamp bind() leaves on it.
  breeze.swap(app, '<div><em>x</em></div>');
  const anon = app.querySelector('em');
  const unbindAnon = breeze.bind(anon, 'counter');
  eq('anonymous region renders', anon.textContent, '5');
  breeze.swap(app, '<div><em>y</em></div>');
  check('anonymous region survived as the same node', app.querySelector('em') === anon);
  breeze.data().count = 6;
  await tick();
  eq('anonymous region updates', app.querySelector('em').textContent, '6');
  unbindAnon();

  // 16. Re-binding the same region to the same template does not stack up
  // duplicate bindings. A view's inline script re-runs on every swap of that
  // view, so a duplicate per render would re-render the region N times for a
  // single change — and each render dispatches breeze:render, which is how
  // that is observed here.
  breeze.swap(app, '<div id="dupe"></div>');
  const dupe = app.querySelector('#dupe');
  let renders = 0;
  const countRenders = (e) => { if (e && e.detail && e.detail.name === 'boxed') renders++; };
  sandbox.window.dispatchEvent = function (e) { countRenders(e); return true; };

  const u1 = breeze.bind(dupe, 'boxed');
  const u2 = breeze.bind(dupe, 'boxed'); // same region, same template
  renders = 0;
  breeze.data().count = 7;
  await tick();
  eq('one region bound twice renders once per change', renders, 1);
  eq('and renders the new value', dupe.textContent, '7');
  u1(); u2();

  // 17. Unbinding stops updates; a removed region does not keep rendering.
  renders = 0;
  breeze.data().count = 8;
  await tick();
  eq('unbound region no longer renders', renders, 0);
  eq('unbound region keeps its last value', dupe.textContent, '7');

  // 18. A binding whose region leaves the document entirely is dropped rather
  // than retried forever against a node nobody can see.
  breeze.swap(app, '<div id="gone"></div>');
  const gone = app.querySelector('#gone');
  breeze.bind(gone, 'boxed');
  breeze.swap(app, '<p>unrelated</p>'); // #gone removed as leftover
  renders = 0;
  breeze.data().count = 9;
  await tick();
  eq('a removed region stops rendering', renders, 0);
  breeze.data().count = 10;
  await tick();
  eq('and stays stopped on later changes', renders, 0);
  sandbox.window.dispatchEvent = function () { return true; };

  // 19. setData that renders a server-embedded view is not undone by the data
  // tag inside that view. The fragment carries the SERVER's copy of the data;
  // reading it back over the value just set would silently revert the update
  // and leave the next mutation rendering stale data.
  breeze.swap(app, '<div id="sd"></div>');
  const sd = app.querySelector('#sd');
  setTag('__breeze_tmpl__', {
    counter: '<b>{{.count}}</b>',
    boxed:   '<b>{{.count}}</b>',
    // Mimics a server-rendered view: content plus its own data tag, carrying
    // the server's (now older) copy of the same field.
    withtag: '<b>{{.count}}</b><script id="__breeze_data__" type="application/json">{"count":1}<\/script>',
  });
  const unbindSd = breeze.bind(sd, 'withtag');
  await breeze.setData({ count: 42 }, sd, 'withtag');
  eq('setData survives the view\'s own embedded data tag', breeze.data().count, 42);
  eq('and rendered the value it was given', sd.querySelector('b').textContent, '42');
  breeze.data().count = 43;
  await tick();
  eq('the next mutation renders from the new value', sd.querySelector('b').textContent, '43');
  unbindSd();

  // 20. A poll whose region is replaced keeps refreshing the live node. Same
  // stale-node failure as a binding, reached through a different trigger.
  setTag('__breeze_data__', { count: 1 });
  breeze.swap(app, '<div id="pollme">start</div>');
  const before = app.querySelector('#pollme');
  let fetched = 0;
  sandbox.fetch = () => {
    fetched++;
    return Promise.resolve({
      ok: true, status: 200, headers: { get: () => null },
      text: () => Promise.resolve('<i>tick ' + fetched + '</i>'),
    });
  };
  breeze.poll('/frag', before, 5);
  await tick();
  breeze.swap(app, '<span id="pollme">replaced</span>');
  const after = app.querySelector('#pollme');
  check('poll region really was replaced', after !== before);
  await new Promise((r) => setTimeout(r, 20));
  breeze.stop(after);
  breeze.stop(before);
  check('poll refreshed the live region, not the detached one',
    after.textContent.indexOf('tick') !== -1, 'got ' + JSON.stringify(after.textContent));
}

async function run() {
  main();
  await reactivity();

  if (failures.length) {
    console.log('FAIL');
    failures.forEach((f) => console.log('  - ' + f));
    process.exit(1);
  }
  console.log('PASS');
}

run().catch((e) => {
  console.log('FAIL\n  - harness error: ' + ((e && e.stack) || e));
  process.exit(1);
});

