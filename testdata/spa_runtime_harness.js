'use strict';

// Harness for the Breeze SPA runtime.
//
// The runtime is a browser script embedded in template.go. This file runs it
// inside a vm context backed by the smallest DOM that lets the public API
// execute for real: the tests below drive window.breeze and assert on
// observable effects (fetches issued, watcher calls, object identity) rather
// than on the runtime's source text.
//
// Usage: node spa_runtime_harness.js <path-to-extracted-runtime.js>

const fs = require('fs');
const vm = require('vm');

const runtimeSrc = fs.readFileSync(process.argv[2], 'utf8');

// ── Failure collection ──────────────────────────────────────────────────────

const failures = [];
function check(name, cond, detail) {
  if (!cond) failures.push(name + (detail ? ': ' + detail : ''));
}
function eq(name, got, want) {
  check(name, Object.is(got, want), 'got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}

// ── Minimal DOM ─────────────────────────────────────────────────────────────

function makeEl(id) {
  return {
    id: id,
    nodeType: 1,
    innerHTML: '',
    textContent: '',
    attributes: [],
    classList: { add() {}, remove() {} },
    // Test fragments never carry state tags or scripts.
    querySelector() { return null; },
    querySelectorAll() { return []; },
    getAttribute() { return null; },
    setAttribute() {},
    hasAttribute() { return false; },
    appendChild() {},
    removeChild() {},
    closest() { return null; },
  };
}

// Elements the runtime looks up by id. __breeze_data__ carries the page JSON
// that breeze.data() hydrates from; there is deliberately no __breeze_tmpl__,
// so re-renders take the server path and become countable fetches.
const dataTag = makeEl('__breeze_data__');
dataTag.textContent = JSON.stringify({
  count: 1,
  user: { name: 'ada', roles: ['admin'] },
  items: [{ id: 1 }],
});

const appEl = makeEl('breeze-app');
const byId = { __breeze_data__: dataTag, 'breeze-app': appEl };

const docListeners = {};
const document = {
  body: Object.assign(makeEl('body'), { appendChild() {} }),
  getElementById(id) { return Object.prototype.hasOwnProperty.call(byId, id) ? byId[id] : null; },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  createElement() { return makeEl(''); },
  addEventListener(type, fn) { (docListeners[type] = docListeners[type] || []).push(fn); },
  removeEventListener(type, fn) {
    const l = docListeners[type] || [];
    const i = l.indexOf(fn);
    if (i !== -1) l.splice(i, 1);
  },
  dispatch(type, ev) { (docListeners[type] || []).slice().forEach((fn) => fn(ev)); },
};

// ── Network ─────────────────────────────────────────────────────────────────
//
// Every request is recorded so tests can assert on how many were made and
// with what. Responses are scripted per test via `routes`.

const calls = [];
let routes = {};

function headersFor(map) {
  const lower = {};
  Object.keys(map || {}).forEach((k) => { lower[k.toLowerCase()] = map[k]; });
  return { get: (k) => (Object.prototype.hasOwnProperty.call(lower, k.toLowerCase()) ? lower[k.toLowerCase()] : null) };
}

function fetchShim(url, opts) {
  opts = opts || {};
  calls.push({ url: url, method: opts.method || 'GET', body: opts.body });
  const r = routes[url] || {};
  return Promise.resolve({
    ok: r.ok !== false,
    status: r.status || 200,
    headers: headersFor(r.headers),
    text: () => Promise.resolve(r.body === undefined ? '<p>ok</p>' : r.body),
  });
}

// ── window / history ────────────────────────────────────────────────────────

const location = { origin: 'https://example.test', pathname: '/', search: '', hash: '', href: '' };

const history = {
  state: null,
  scrollRestoration: 'auto',
  replaceState(s, _t, url) { this.state = s; if (url) applyUrl(url); },
  pushState(s, _t, url) { this.state = s; if (url) applyUrl(url); },
};

function applyUrl(url) {
  const u = new URL(url, location.origin);
  location.pathname = u.pathname;
  location.search = u.search;
  location.hash = u.hash;
  location.href = u.href;
}

const winListeners = {};
const window = {
  location: location,
  scrollY: 0,
  scrollTo() {},
  addEventListener(type, fn) { (winListeners[type] = winListeners[type] || []).push(fn); },
  removeEventListener() {},
  dispatchEvent() { return true; },
  WebSocket: function () {},
};

class CustomEventShim {
  constructor(type, init) { this.type = type; this.detail = (init || {}).detail; }
}

class AbortControllerShim {
  constructor() { this.signal = { aborted: false }; }
  abort() { this.signal.aborted = true; }
}

const sandbox = {
  window, document, history, location,
  fetch: fetchShim,
  CustomEvent: CustomEventShim,
  AbortController: AbortControllerShim,
  HTMLFormElement: function HTMLFormElement() {},
  WebSocket: function WebSocket() {},
  console,
  setTimeout, clearTimeout, setInterval, clearInterval,
  requestAnimationFrame: (fn) => setTimeout(fn, 0),
  URL, URLSearchParams, FormData: function FormData() {},
  Promise, Map, Set, WeakMap, Proxy, Reflect, Object, Array, JSON, String, Number, Math, isNaN, RegExp, Error,
};
sandbox.globalThis = sandbox;
HTMLFormElementSubmitProbe(sandbox);

function HTMLFormElementSubmitProbe(sb) {
  // Records whether the fallback reached the prototype method rather than a
  // shadowed property on the element.
  sb.HTMLFormElement.prototype.submit = function () { sb.__protoSubmitCalls = (sb.__protoSubmitCalls || 0) + 1; };
}

vm.createContext(sandbox);
vm.runInContext(runtimeSrc, sandbox, { filename: 'breeze-runtime.js' });

const breeze = sandbox.window.breeze;
check('runtime exposes window.breeze', !!breeze);

// ── Helpers ─────────────────────────────────────────────────────────────────

const tick = () => new Promise((r) => setTimeout(r, 5));

function resetNet(newRoutes) {
  calls.length = 0;
  routes = newRoutes || {};
}

// ── Tests ───────────────────────────────────────────────────────────────────

async function main() {
  // 1. Public API surface is unchanged.
  ['fetch', 'poll', 'stop', 'swap', 'navigate', 'data', 'setData', 'render',
    'watch', 'list', 'bind', 'invalidate', 'invalidateAll', 'on', 'ws',
  ].forEach((k) => check('breeze.' + k + ' exists', typeof breeze[k] === 'function'));

  // 2. Nested reads return one stable proxy instead of a fresh one each time.
  const d = breeze.data();
  check('nested object identity is stable', d.user === d.user);
  check('nested array identity is stable', d.items === d.items);
  check('deep identity is stable', d.user.roles === d.user.roles);

  // 3. Writing an identical value is not a change and must not re-render.
  let notifications = 0;
  const unwatch = breeze.watch(() => { notifications++; });

  d.count = 1; // same value as the embedded data
  await tick();
  eq('no-op write does not notify', notifications, 0);

  // 4. A real change does notify.
  d.count = 2;
  await tick();
  eq('real write notifies', notifications, 1);

  // 5. Several mutations in one tick still collapse into a single pass.
  notifications = 0;
  d.count = 3;
  d.count = 4;
  d.user.name = 'grace';
  d.items.push({ id: 2 });
  await tick();
  eq('same-tick mutations batch into one pass', notifications, 1);

  // 6. Deleting an absent property is not a change.
  notifications = 0;
  delete d.nothingHere;
  await tick();
  eq('deleting absent property does not notify', notifications, 0);

  unwatch();

  // 7. Bindings on the same template+data share one server render.
  const t1 = makeEl('t1'); const t2 = makeEl('t2'); const t3 = makeEl('t3');
  resetNet();
  breeze.bind(t1, 'card');
  breeze.bind(t2, 'card');
  breeze.bind(t3, 'card');
  await tick();
  const bindCalls = calls.length; // one per bind() is expected: they are separate calls

  resetNet();
  breeze.data().count = 99; // one state change fans out to three bindings
  await tick();
  const renderCalls = calls.filter((c) => c.url === '/breeze/render').length;
  eq('three bindings share one render request', renderCalls, 1);
  check('bind() itself still renders each target', bindCalls === 3, 'got ' + bindCalls);

  // 8. Delegated handlers survive a non-Element event target.
  let handlerRuns = 0;
  let handlerThis = null;
  const off = breeze.on('.thing', 'click', function () { handlerRuns++; handlerThis = this; });

  // A target with no element anywhere above it must not throw.
  let threw = null;
  try {
    document.dispatch('click', { target: { nodeType: 3, parentElement: null } });
    document.dispatch('click', { target: null });
    document.dispatch('click', { target: { nodeType: 9 } }); // document, has no closest()
  } catch (e) { threw = e; }
  check('delegation tolerates non-element targets', threw === null, threw && threw.message);
  eq('no spurious handler run', handlerRuns, 0);

  // A matching element fires and the handler receives it as `this`.
  const hit = makeEl('x');
  hit.closest = (sel) => (sel === '.thing' ? hit : null);
  document.dispatch('click', { target: hit });
  eq('matching element still fires', handlerRuns, 1);
  check('handler receives the matched element', handlerThis === hit);

  // The case that actually regressed: browsers report a click on a button's
  // label as landing on the text node inside it. The handler must still fire
  // for the element that contains that text.
  handlerRuns = 0;
  handlerThis = null;
  document.dispatch('click', { target: { nodeType: 3, parentElement: hit } });
  eq('click on text inside a match still fires', handlerRuns, 1);
  check('text-node click resolves to the containing element', handlerThis === hit);
  off();


  // 9. Route cache respects Cache-Control.
  applyUrl('/');
  breeze.invalidateAll();
  resetNet({ '/private': { headers: { 'Cache-Control': 'private, no-store' } } });
  breeze.navigate('/private'); await tick();
  applyUrl('/');
  breeze.navigate('/private'); await tick();
  eq('private response is not replayed from cache', calls.filter((c) => c.url === '/private').length, 2);

  // A public response is cached, so the second visit does not refetch.
  applyUrl('/');
  breeze.invalidateAll();
  resetNet({ '/public': { headers: {} } });
  breeze.navigate('/public'); await tick();
  applyUrl('/');
  breeze.navigate('/public'); await tick();
  eq('public response is served from cache', calls.filter((c) => c.url === '/public').length, 1);

  // 10. An identity change drops everything cached for the previous user.
  //
  // The identity flip has to arrive on some *other* route, and nothing may
  // invalidate /acct by hand — otherwise the refetch below is guaranteed by
  // the test itself and says nothing about the epoch logic. So: cache /acct
  // as user-1, then visit /login (uncached, and the only route that reports
  // user-2). That response is what must clear the cache, forcing the second
  // /acct visit back onto the network.
  applyUrl('/');
  breeze.invalidateAll();
  resetNet({
    '/acct':  { headers: { 'X-Breeze-Auth': 'user-1' } },
    '/login': { headers: { 'X-Breeze-Auth': 'user-2' } },
  });

  breeze.navigate('/acct'); await tick();          // cached, epoch = user-1
  applyUrl('/');
  eq('identity: /acct cached under the first user',
    calls.filter((c) => c.url === '/acct').length, 1);

  breeze.navigate('/login'); await tick();         // epoch flips -> cache cleared
  applyUrl('/');

  breeze.navigate('/acct'); await tick();          // must hit the network again
  eq('cache does not survive an identity change',
    calls.filter((c) => c.url === '/acct').length, 2);

  // A stable identity must NOT keep dropping the cache, or every response
  // carrying the header would defeat caching entirely.
  applyUrl('/');
  breeze.invalidateAll();
  resetNet({
    '/acct':  { headers: { 'X-Breeze-Auth': 'user-2' } },
    '/other': { headers: { 'X-Breeze-Auth': 'user-2' } },
  });
  breeze.navigate('/acct');  await tick();
  applyUrl('/');
  breeze.navigate('/other'); await tick();
  applyUrl('/');
  breeze.navigate('/acct');  await tick();
  eq('an unchanged identity leaves the cache alone',
    calls.filter((c) => c.url === '/acct').length, 1);

  // ── Report ────────────────────────────────────────────────────────────────
  if (failures.length) {
    console.log('FAIL');
    failures.forEach((f) => console.log('  - ' + f));
    process.exit(1);
  }
  console.log('PASS');
}

main().catch((e) => { console.log('FAIL\n  - harness error: ' + (e && e.stack || e)); process.exit(1); });
