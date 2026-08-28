// Fleet playback + topology harness.
//
// Runs the *shipped* dashboard/templates/public/dashboard.js under a stub DOM
// with a controllable clock and a controllable requestAnimationFrame queue.
//
// Why a harness and not source assertions: all four bugs this pins are timing
// and state bugs. "Playback stops after two spans", "0.0x silently means 1x",
// "the speed menu applies one step late", and "two render paths compound into
// two animation loops" are all invisible in the source text and all obvious
// the moment you can advance a clock and count timers. Asserting on source
// would pass against code that still misbehaves.
//
// The DOM here is a stand-in, but the code under test is the real file and the
// assertions are about behaviour: which event name a control was bound with,
// how many timers get armed, how playback's index progresses, and how many
// animation frames are ever in flight at once.
//
// Usage: node fleet_playback_harness.js <path-to-dashboard.js>

'use strict';

const fs = require('fs');

const failures = [];
function check(name, cond, detail) {
  if (cond) { console.log('  ok   ' + name); return; }
  failures.push(name + (detail ? ' — ' + detail : ''));
  console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
}
function eq(name, got, want) {
  check(name, got === want, 'got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
}

// ── Fake clock ─────────────────────────────────────────────────────────
// Playback schedules itself with setTimeout, so the only way to observe
// "does it keep going" is to own the clock.
let now = 1000;
let timerSeq = 0;
let timers = new Map();       // id -> {due, fn}
let timersArmed = 0;          // cumulative, so a test can assert none was armed

function setTimeoutStub(fn, ms) {
  const id = ++timerSeq;
  timersArmed++;
  timers.set(id, { due: now + (typeof ms === 'number' ? ms : 0), fn: fn });
  return id;
}
function clearTimeoutStub(id) { timers.delete(id); }

// advance moves the clock and fires due timers in due order, including
// timers armed by timers that just fired (that is exactly the chain
// playback relies on).
function advance(ms) {
  const target = now + ms;
  let guard = 0;
  for (;;) {
    let next = null;
    for (const [id, t] of timers) {
      if (t.due <= target && (next === null || t.due < timers.get(next).due)) next = id;
    }
    if (next === null || guard++ > 10000) break;
    const t = timers.get(next);
    timers.delete(next);
    now = Math.max(now, t.due);
    t.fn();
  }
  now = target;
}

// ── Fake animation frames ──────────────────────────────────────────────
let rafSeq = 0;
let rafQueue = [];            // pending callbacks
let rafPeak = 0;              // high-water mark of pending frames
function requestAnimationFrameStub(fn) {
  rafQueue.push(fn);
  if (rafQueue.length > rafPeak) rafPeak = rafQueue.length;
  return ++rafSeq;
}
// drainFrames runs exactly the frames pending right now. Frames those
// frames schedule are left queued for the next drain, which is what lets
// us see whether one frame begets one frame (correct) or several.
function drainFrames() {
  const batch = rafQueue;
  rafQueue = [];
  batch.forEach(function (fn) { fn(); });
  return batch.length;
}

// ── Minimal DOM ────────────────────────────────────────────────────────
// Only what the fleet code path touches. innerHTML is not parsed properly;
// instead the markup is scanned for the handful of hooks the code then
// queries back out of it (data-action controls, .t-step rows, the caret).
// That is enough to answer the question this harness exists to answer —
// which event name was bound to which control, and which step is marked
// active — without a full HTML parser.
function El(tag) {
  this.tagName = String(tag || 'div').toUpperCase();
  this.children = [];
  this.dataset = {};
  this.style = {};
  this._html = '';
  this._synth = [];
  this._handlers = {};
  this._classes = new Set();
  this.value = '';
  this.textContent = '';
  const self = this;
  this.classList = {
    add: function (c) { self._classes.add(c); },
    remove: function (c) { self._classes.delete(c); },
    contains: function (c) { return self._classes.has(c); },
    toggle: function (c, on) {
      const want = (on === undefined) ? !self._classes.has(c) : !!on;
      if (want) self._classes.add(c); else self._classes.delete(c);
      return want;
    }
  };
}
Object.defineProperty(El.prototype, 'className', {
  get: function () { return Array.from(this._classes).join(' '); },
  set: function (v) { this._classes = new Set(String(v || '').split(/\s+/).filter(Boolean)); }
});
Object.defineProperty(El.prototype, 'innerHTML', {
  get: function () { return this._html; },
  set: function (v) {
    this._html = String(v == null ? '' : v);
    this._synth = [];
    const self = this;
    // Controls: <button data-action="play">, <select data-action="speed">
    const ctl = /<(\w+)([^>]*\bdata-action="([^"]+)"[^>]*)>/g;
    let m;
    while ((m = ctl.exec(this._html)) !== null) {
      const e = new El(m[1]);
      e.dataset.action = m[3];
      if (e.tagName === 'SELECT') {
        // The selected option is what a real select would report.
        const sel = /<option value="([^"]*)"[^>]*\bselected/.exec(self._html);
        e.value = sel ? sel[1] : '';
      }
      self._synth.push(e);
    }
    // Timeline step rows, so renderFleetPlayback has something to mark.
    const step = /class="t-step[^"]*"/g;
    while (step.exec(this._html) !== null) {
      const e = new El('div');
      e.classList.add('t-step');
      self._synth.push(e);
    }
    if (/accordion-caret/.test(this._html)) {
      const e = new El('span');
      e.classList.add('accordion-caret');
      self._synth.push(e);
    }
  }
});
El.prototype.appendChild = function (n) { n._parent = this; this.children.push(n); return n; };
// reconcileList orders rows with insertBefore/firstChild/nextSibling and
// drops stale ones with remove(), so the stub has to model parentage for
// any assertion about list contents to mean anything.
El.prototype.insertBefore = function (n, ref) {
  if (n._parent) n.remove();
  n._parent = this;
  const at = ref ? this.children.indexOf(ref) : -1;
  if (at < 0) this.children.push(n); else this.children.splice(at, 0, n);
  return n;
};
El.prototype.remove = function () {
  const p = this._parent;
  if (!p) return;
  const at = p.children.indexOf(this);
  if (at >= 0) p.children.splice(at, 1);
  this._parent = null;
};
Object.defineProperty(El.prototype, 'firstChild', {
  get: function () { return this.children[0] || null; }
});
Object.defineProperty(El.prototype, 'nextSibling', {
  get: function () {
    if (!this._parent) return null;
    const sibs = this._parent.children;
    return sibs[sibs.indexOf(this) + 1] || null;
  }
});

// The el() builder sets arbitrary attributes (role, tabindex), so the stub
// has to accept them; id is mirrored because matches() selects on '#id'.
El.prototype.setAttribute = function (k, v) {
  this._attrs = this._attrs || {};
  this._attrs[k] = String(v);
  if (k === 'id') this.id = String(v);
};
El.prototype.getAttribute = function (k) {
  return (this._attrs && this._attrs[k] !== undefined) ? this._attrs[k] : null;
};
El.prototype.addEventListener = function (ev, fn) {
  (this._handlers[ev] = this._handlers[ev] || []).push(fn);
};

El.prototype.dispatch = function (ev) {
  (this._handlers[ev] || []).forEach(function (fn) { fn({ stopPropagation: function () {} }); });
  return (this._handlers[ev] || []).length;
};
El.prototype.boundEvents = function () { return Object.keys(this._handlers); };
El.prototype.matches = function (sel) {
  sel = String(sel).trim();
  // Compound descendant selectors are reduced to their last simple part;
  // the fleet code only uses them to reach controls inside a container.
  if (sel.indexOf(' ') >= 0) sel = sel.split(/\s+/).pop();
  if (sel === '[data-key]') return this.dataset.key !== undefined;
  let m = /^\[data-action="([^"]+)"\]$/.exec(sel);
  if (m) return this.dataset.action === m[1];
  if (sel === '[data-action]') return this.dataset.action !== undefined;
  if (sel[0] === '.') return this._classes.has(sel.slice(1));
  if (sel[0] === '#') return this.id === sel.slice(1);
  return this.tagName === sel.toUpperCase();
};
El.prototype._walk = function (out) {
  const self = this;
  this._synth.forEach(function (s) { out.push(s); });
  this.children.forEach(function (c) { out.push(c); if (c._walk) c._walk(out); });
  return out;
};
El.prototype.querySelectorAll = function (sel) {
  return this._walk([]).filter(function (n) { return n.matches && n.matches(sel); });
};
El.prototype.querySelector = function (sel) {
  return this.querySelectorAll(sel)[0] || null;
};

// A canvas whose 2D context accepts everything and reports plausible
// measurements.
//
// It also records the two things the topology's *shape* is asserted on:
// stroked paths (with their endpoints and how many curve segments they
// carry) and drawn text (with the font active at the time). Everything
// else is a noop.
//
// Recording is what makes "every edge is drawn as a request AND a response
// arc" a testable claim rather than a visual one. A single-segment stroked
// path is an edge; a round-rect (node card, label pill) carries four, so
// the two are told apart by segment count instead of by guessing. Text is
// tagged with its font because the 10px face is only ever used for edge
// timing labels, which separates them from the 12px node names.
function makeCanvas() {
  const c = new El('canvas');
  c.clientWidth = 900;
  c.width = 0; c.height = 0;
  const noop = function () {};
  const draws = {
    paths: [], labels: [],
    reset: function () { this.paths = []; this.labels = []; },
    // Single-segment stroked paths, i.e. the edge arcs.
    arcs: function () { return this.paths.filter(function (p) { return p.curves === 1; }); },
    // Edge timing labels, by their distinctive 10px face.
    edgeLabels: function () {
      return this.labels.filter(function (l) { return /10px/.test(l.font); })
        .map(function (l) { return l.text; });
    }
  };
  c.draws = draws;
  c.getContext = function () {
    let path = null;
    let font = '';
    return {
      get font() { return font; },
      set font(v) { font = String(v || ''); },
      save: noop, restore: noop, clip: noop,
      lineTo: noop, arc: noop, ellipse: noop, rect: noop,
      closePath: noop, fill: noop, fillRect: noop,
      clearRect: noop, setLineDash: noop, setTransform: noop, scale: noop,
      translate: noop,
      beginPath: function () { path = { from: null, to: null, curves: 0 }; },
      moveTo: function (x, y) { if (path && !path.from) path.from = { x: x, y: y }; },
      quadraticCurveTo: function (cx, cy, x, y) {
        if (!path) return;
        path.curves++;
        path.to = { x: x, y: y };
      },
      // Only stroked paths are recorded: filled-only paths are arrowheads
      // and glow blobs, which are decoration on an edge already counted.
      stroke: function () { if (path) draws.paths.push(path); },
      fillText: function (t) { draws.labels.push({ text: String(t), font: font }); },
      measureText: function (s) { return { width: String(s).length * 6.5 }; },
      createRadialGradient: function () { return { addColorStop: noop }; },
      createLinearGradient: function () { return { addColorStop: noop }; }
    };
  };
  return c;
}


const registry = {};
const canvas = makeCanvas();
registry['#fleet-topology'] = canvas;
registry['#fleet-topology-meta'] = new El('div');

const documentStub = {
  documentElement: {
    _attrs: {},
    getAttribute: function (k) { return this._attrs[k] || null; },
    setAttribute: function (k, v) { this._attrs[k] = v; }
  },
  querySelector: function (sel) { return registry[sel] || null; },
  querySelectorAll: function (sel) {
    const hit = registry[sel];
    return hit ? [hit] : [];
  },
  getElementById: function () { return null; },
  createElement: function (tag) { return new El(tag); },
  createTextNode: function (t) { const e = new El('#text'); e.textContent = t; return e; },
  addEventListener: noopFn
};
function noopFn() {}

// A populated graph matching the trace fixture below. The renderer draws a
// zero-state and deliberately does not animate when there are no nodes, so
// any test about animation frames has to have real nodes to animate.
function makeTopology() {
  return {
    nodes: [
      { service: 'gateway', status: 'up', rps: 40, error_rate: 0 },
      { service: 'auth-service', status: 'up', rps: 40, error_rate: 0 },
      { service: 'orders-service', status: 'up', rps: 20, error_rate: 0 },
      { service: 'billing-service', status: 'up', rps: 20, error_rate: 0 },
      { service: 'notifications-service', status: 'up', rps: 20, error_rate: 0 }
    ],
    edges: [
      { caller: 'gateway', callee: 'auth-service', count: 100, p50_ms: 5, p95_ms: 9, p99_ms: 12, error_rate: 0 },
      { caller: 'gateway', callee: 'orders-service', count: 100, p50_ms: 8, p95_ms: 14, p99_ms: 20, error_rate: 0 },
      { caller: 'orders-service', callee: 'billing-service', count: 100, p50_ms: 6, p95_ms: 10, p99_ms: 15, error_rate: 0 },
      { caller: 'orders-service', callee: 'notifications-service', count: 100, p50_ms: 3, p95_ms: 6, p99_ms: 9, error_rate: 0 }
    ]
  };
}

// fetch: only the endpoints the fleet path uses. The trace body is what
// each test installs on traceResponse.
let traceResponse = null;
function fetchStub(url) {
  let data = {};
  if (/fleet\/traces\//.test(url)) data = traceResponse;
  else if (/fleet\/traces/.test(url)) data = { traces: [], has_more: false };
  else if (/fleet\/(services|incidents)/.test(url)) data = [];
  else if (/fleet\/topology/.test(url)) data = makeTopology();
  return Promise.resolve({
    status: 200, ok: true,
    json: function () { return Promise.resolve(data); }
  });
}

// ── Load the shipped dashboard.js ──────────────────────────────────────
// The fleet functions live inside the file's IIFE and are not exported, so
// the harness appends one export line to the source it evaluates. The code
// under test is unmodified; only the evaluated copy gains a seam.
const target = process.argv[2];
if (!target) { console.error('usage: node fleet_playback_harness.js <dashboard.js>'); process.exit(2); }
let source = fs.readFileSync(target, 'utf8');

const exportAnchor = 'window.BreezeDash = {initPage: initPage, api: api, apiPost: apiPost, S: S};';
if (source.indexOf(exportAnchor) < 0) {
  console.error('FAIL: export anchor not found in dashboard.js — harness needs updating');
  process.exit(1);
}
source = source.replace(exportAnchor, exportAnchor + '\nwindow.__fleetInternals = {' +
  'S: S, toggleFleetTrace: toggleFleetTrace, fleetPlaybackAction: fleetPlaybackAction,' +
  'fleetSpanDelay: fleetSpanDelay, fleetTopoNeedsFrame: fleetTopoNeedsFrame,' +
  'fleetTopoLayout: fleetTopoLayout, renderFleetTopology: renderFleetTopology,' +
  'fleetFlattenTrace: fleetFlattenTrace, FLEET_USER_KEY: FLEET_USER_KEY,' +
  'fleetHopTimings: fleetHopTimings,' +

  'renderFleetTraces: renderFleetTraces, initFleet: initFleet, loadFleet: loadFleet};');


const windowStub = {
  devicePixelRatio: 1,
  requestAnimationFrame: requestAnimationFrameStub,
  matchMedia: function () { return { matches: false }; },
  location: { pathname: '/dashboard', protocol: 'http:', host: 'localhost' },
  localStorage: { getItem: function () { return null; }, setItem: noopFn },
  getComputedStyle: function () { return { getPropertyValue: function () { return ''; } }; },
  WebSocket: function () { this.close = noopFn; },
  addEventListener: noopFn
};
windowStub.window = windowStub;
windowStub.document = documentStub;
windowStub.fetch = fetchStub;
windowStub.setTimeout = setTimeoutStub;
windowStub.clearTimeout = clearTimeoutStub;
// setInterval is counted, not stubbed away: "the page only refreshes on a
// tab change" is a claim about whether a poller was ever armed, and how
// many were armed across repeated visits to the page.
let intervalsArmed = 0;
let intervalFns = [];
function setIntervalStub(fn) {
  intervalsArmed++;
  intervalFns.push(fn);
  return intervalsArmed;
}
windowStub.setInterval = setIntervalStub;
windowStub.clearInterval = noopFn;
windowStub.Date = Date;


const vm = require('vm');
const sandbox = {
  window: windowStub,
  document: documentStub,
  location: windowStub.location,
  fetch: fetchStub,
  setTimeout: setTimeoutStub,
  clearTimeout: clearTimeoutStub,
  setInterval: windowStub.setInterval,
  clearInterval: noopFn,
  requestAnimationFrame: requestAnimationFrameStub,
  getComputedStyle: windowStub.getComputedStyle,
  WebSocket: windowStub.WebSocket,
  navigator: { clipboard: { writeText: function () { return Promise.resolve(); } } },
  console: console,
  Math: Math, JSON: JSON, Promise: Promise, Array: Array, Object: Object,
  String: String, Number: Number, Boolean: Boolean, Set: Set, Map: Map,
  isNaN: isNaN, isFinite: isFinite, parseInt: parseInt, parseFloat: parseFloat,
  Infinity: Infinity, NaN: NaN, Error: Error, RegExp: RegExp
};
// Date.now must follow the fake clock: the response-leg beat and the glow
// phase both read it, and a real clock would make those untestable.
sandbox.Date = new Proxy(Date, {
  get: function (t, p) { return p === 'now' ? function () { return now; } : t[p]; },
  construct: function (t, args) { return new t(...args); },
  apply: function (t, self, args) { return t.apply(self, args); }
});

vm.createContext(sandbox);
vm.runInContext(source, sandbox, { filename: 'dashboard.js' });

const F = windowStub.__fleetInternals;
if (!F) { console.error('FAIL: internals not exported'); process.exit(1); }

// ── Fixture ────────────────────────────────────────────────────────────
// A five-span trace: gateway calls auth, then orders, which calls billing
// and notifications. Durations are equal so expected delays are easy to
// reason about at a given speed.
function makeTrace() {
  return {
    trace_id: 'abc123def456abc123def456abc123de',
    summary: 'gateway served POST /orders in 120ms',
    roots: [{
      service: 'gateway', method: 'POST', route: '/orders', duration_ms: 120, start_ns: 1e6, tags: {},
      children: [
        { service: 'auth-service', method: 'GET', route: '/verify', duration_ms: 100, start_ns: 2e6, tags: {}, children: [] },
        {
          service: 'orders-service', method: 'POST', route: '/create', duration_ms: 100, start_ns: 3e6, tags: {},
          children: [
            { service: 'billing-service', method: 'POST', route: '/charge', duration_ms: 100, start_ns: 4e6, tags: {}, children: [] },
            { service: 'notifications-service', method: 'POST', route: '/send', duration_ms: 100, start_ns: 5e6, tags: {}, children: [] }
          ]
        }
      ]
    }]
  };
}

// buildRow constructs the accordion row toggleFleetTrace expects, then
// opens it so the real code renders the controls and binds their handlers.
async function buildRow() {
  const row = new El('div');
  row.classList.add('accordion-item');
  const head = new El('div');
  head.classList.add('accordion-head');
  head.innerHTML = '<span class="accordion-caret">&#9656;</span>';
  const body = new El('div');
  body.classList.add('accordion-body');
  row.appendChild(head);
  row.appendChild(body);

  traceResponse = makeTrace();
  F.S.fleet.playback = null;
  // Playback's own re-renders read the stored topology, so it must be
  // populated for the animation assertions to be about anything.
  F.S.fleet.topology = makeTopology();
  F.toggleFleetTrace(row, traceResponse.trace_id);

  // The detail fetch resolves through fetch -> .json() -> .then(render), so
  // the controls only exist after that whole chain has drained. Three ticks
  // landed mid-chain and snapshotted an empty control set, which read as
  // "the play button does not exist" rather than "the harness looked early".
  for (let i = 0; i < 25; i++) await Promise.resolve();


  const controls = {};
  body.querySelectorAll('[data-action]').forEach(function (c) { controls[c.dataset.action] = c; });
  return { row: row, body: body, controls: controls };
}
function pb() { return F.S.fleet.playback; }

// ── Tests ──────────────────────────────────────────────────────────────
async function main() {
  console.log('fleet playback harness');

  // 1. Continuous playback.
  //
  // Advancing used to recurse through fleetPlaybackAction(...,'play'), and
  // 'play' toggles running — so the second span turned playback off and it
  // stopped two spans in. This walks all five spans on one press.
  console.log('\n[1] one play press walks the whole trace');
  {
    const ui = await buildRow();
    check('play control exists', !!ui.controls.play);
    check('five spans flattened', pb() ? pb().spans.length === 5 : false,
      'got ' + (pb() ? pb().spans.length : 'no playback'));
    ui.controls.play.dispatch('click');
    eq('running after press', pb().running, true);
    eq('starts at first span', pb().index, 0);
    advance(5000);
    eq('reached last span', pb().index, 4);
    eq('stopped at the end', pb().running, false);
    check('marked a finish time', pb().finishedAt > 0);
    // The specific regression: index must pass 1. Old code stalled there.
    check('did not stall two spans in', pb().index > 1, 'index ' + pb().index);
  }

  // 2. Speed is a divisor, and 0.0x is reachable.
  //
  // duration/(speed||1) made 0 falsy, so "hold" silently ran at 1x — the
  // one preset the menu could not select.
  console.log('\n[2] 0.0x holds instead of silently running at 1x');
  {
    const ui = await buildRow();
    const at1x = F.fleetSpanDelay(Object.assign({}, pb(), { index: 0, speed: 1 }));
    const at05 = F.fleetSpanDelay(Object.assign({}, pb(), { index: 0, speed: 0.5 }));
    const at005 = F.fleetSpanDelay(Object.assign({}, pb(), { index: 0, speed: 0.05 }));
    eq('0.5x takes twice as long', at05, at1x * 2);
    eq('0.05x takes twenty times as long', at005, at1x * 20);
    const held = F.fleetSpanDelay(Object.assign({}, pb(), { index: 0, speed: 0 }));
    eq('0.0x reports Infinity', held, Infinity);

    ui.controls.speed.value = '0';
    ui.controls.speed.dispatch('change');
    eq('speed stored as 0', pb().speed, 0);
    ui.controls.play.dispatch('click');
    const armedBefore = timersArmed;
    advance(60000);
    eq('no timer armed while held', timersArmed, armedBefore);
    eq('held on the first span', pb().index, 0);
    eq('still considered running', pb().running, true);
    check('still animating while held', F.fleetTopoNeedsFrame(pb()) === true);

    // Releasing the hold resumes from where it stopped, without a re-press.
    ui.controls.speed.value = '1';
    ui.controls.speed.dispatch('change');
    advance(5000);
    eq('resumed to the last span', pb().index, 4);
  }

  // 3. The speed <select> is bound to change, not click.
  //
  // Bound to click it reported the value selected BEFORE the user picked,
  // so every change applied one step late and keyboard selection never
  // applied at all.
  console.log('\n[3] select binds change; buttons bind click');
  {
    const ui = await buildRow();
    eq('select tag', ui.controls.speed.tagName, 'SELECT');
    check('speed bound to change', ui.controls.speed.boundEvents().indexOf('change') >= 0,
      'bound: ' + JSON.stringify(ui.controls.speed.boundEvents()));
    check('speed NOT bound to click', ui.controls.speed.boundEvents().indexOf('click') < 0,
      'bound: ' + JSON.stringify(ui.controls.speed.boundEvents()));
    check('play bound to click', ui.controls.play.boundEvents().indexOf('click') >= 0);
    check('prev bound to click', ui.controls.prev.boundEvents().indexOf('click') >= 0);
    check('next bound to click', ui.controls.next.boundEvents().indexOf('click') >= 0);

    // A change applies the value picked now, not the one picked last time.
    ui.controls.speed.value = '0.5';
    ui.controls.speed.dispatch('change');
    eq('applied 0.5 immediately', pb().speed, 0.5);
    ui.controls.speed.value = '0.1';
    ui.controls.speed.dispatch('change');
    eq('applied 0.1 immediately, not 0.5', pb().speed, 0.1);
  }

  // 4. One animation loop, no matter how many callers render.
  //
  // Each render used to schedule its own frame, so a 5s poll tick landing
  // mid-playback compounded into two self-scheduling loops: the animation
  // sped up and burned CPU the longer it ran.
  console.log('\n[4] overlapping renders keep exactly one frame in flight');
  {
    const ui = await buildRow();
    ui.controls.play.dispatch('click');
    // Drain rather than clear: the renderer tracks its own "frame pending"
    // flag, and emptying the queue behind its back would leave it believing
    // a frame is still in flight. Running the frame lets the code clear that
    // flag itself, so the harness and the code agree on the state.
    drainFrames();
    rafPeak = rafQueue.length;
    eq('running playback keeps one frame queued', rafQueue.length, 1);

    // Poll ticks landing on top of playback's own already-pending frame.
    // This is the compounding bug exactly: with per-render scheduling these
    // six would stack up to seven pending frames and each would go on to
    // schedule its own successor.
    for (let i = 0; i < 6; i++) F.renderFleetTopology(F.S.fleet.topology, F.S.fleet.services, pb());
    eq('six extra renders add no extra frames', rafQueue.length, 1);


    for (let i = 0; i < 8; i++) {
      const ran = drainFrames();
      check('frame ' + i + ' ran exactly one callback', ran <= 1, 'ran ' + ran);
      F.renderFleetTopology(F.S.fleet.topology, F.S.fleet.services, pb());
      check('never more than one pending after frame ' + i, rafQueue.length <= 1,
        'pending ' + rafQueue.length);
    }
    check('peak pending frames stayed at one', rafPeak <= 1, 'peak ' + rafPeak);

    // A finished, idle playback must eventually stop asking for frames,
    // otherwise the graph animates forever over a static trace.
    advance(5000);
    rafQueue = [];
    F.renderFleetTopology(F.S.fleet.topology, F.S.fleet.services, pb());
    eq('idle playback stops requesting frames', rafQueue.length, 0);
  }

  // 5. Layered layout, so an edge's direction carries meaning.
  console.log('\n[5] layout puts the user left of the entry, callees to the right');
  {
    const nodes = [{ service: 'gateway' }, { service: 'auth-service' }, { service: 'orders-service' }, { service: 'lonely-service' }];
    const edges = [
      { caller: 'gateway', callee: 'auth-service' },
      { caller: 'gateway', callee: 'orders-service' }
    ];
    const pos = F.fleetTopoLayout(nodes, edges, 'gateway', 900, 340);
    const user = pos[F.FLEET_USER_KEY];
    check('user node placed', !!user);
    check('user is left of the entry', user.x < pos['gateway'].x);
    check('callee is right of its caller', pos['auth-service'].x > pos['gateway'].x);
    eq('entry at depth 0', pos['gateway'].depth, 0);
    eq('callee at depth 1', pos['auth-service'].depth, 1);
    check('siblings share a column', pos['auth-service'].x === pos['orders-service'].x);
    check('siblings do not overlap', pos['auth-service'].y !== pos['orders-service'].y);
    // A service nothing was observed calling is parked, not dropped.
    check('unreached service still placed', !!pos['lonely-service']);
    check('unreached service parked rightmost', pos['lonely-service'].x > pos['auth-service'].x);

    // A cycle must not hang the layout.
    const cyc = F.fleetTopoLayout(
      [{ service: 'a' }, { service: 'b' }],
      [{ caller: 'a', callee: 'b' }, { caller: 'b', callee: 'a' }],
      'a', 900, 340);
    check('cycle terminates and places both', !!cyc['a'] && !!cyc['b']);
  }

  // 6. Stepping is independent of playing.
  console.log('\n[6] prev/next step without leaving playback running');
  {
    const ui = await buildRow();
    ui.controls.next.dispatch('click');
    eq('next advances one span', pb().index, 0);
    ui.controls.next.dispatch('click');
    eq('next advances again', pb().index, 1);
    eq('stepping does not start playback', pb().running, false);
    ui.controls.prev.dispatch('click');
    eq('prev goes back', pb().index, 0);
    ui.controls.prev.dispatch('click');
    eq('prev floors before the first span', pb().index, -1);
    ui.controls.prev.dispatch('click');
    eq('prev does not run past the floor', pb().index, -1);
    for (let i = 0; i < 12; i++) ui.controls.next.dispatch('click');
    eq('next caps at the last span', pb().index, 4);
  }

  // 7. Rendering an empty fleet must not throw or animate.
  console.log('\n[7] empty topology renders a zero-state');
  {
    F.S.fleet.playback = null;
    let threw = null;
    try { F.renderFleetTopology({ nodes: [], edges: [] }, [], null); } catch (e) { threw = e; }
    check('no throw on empty topology', threw === null, threw && threw.message);
    eq('meta reports zero', registry['#fleet-topology-meta'].textContent, '0 nodes · 0 edges');
  }

  // 8. The page refreshes itself, and re-visiting it does not stack pollers.
  //
  // Two separate defects lived here. Fleet View had no self-refresh at all,
  // so data only changed when the user switched tabs and initFleet re-ran.
  // Adding one naively then armed a fresh interval on *every* visit, with
  // none ever cleared: five visits meant five pollers interleaving calls to
  // loadFleet() and trampling the shared cursor/traces state between them.
  console.log('\n[8] exactly one poller, however many times the page is opened');
  {
    registry['#fleet-traces'] = new El('div');
    registry['#fleet-cards'] = new El('div');
    const before = intervalsArmed;
    F.S.page = 'fleet';
    F.initFleet();
    const afterFirst = intervalsArmed;
    check('opening the page arms a refresh', afterFirst > before,
      'armed ' + (afterFirst - before));
    eq('exactly one poller for the first visit', afterFirst - before, 1);

    for (let i = 0; i < 5; i++) F.initFleet();
    eq('re-opening arms no further pollers', intervalsArmed, afterFirst);

    // The poller must actually fetch, not just exist.
    const poll = intervalFns[intervalFns.length - 1];
    F.S.fleet.loading = false;
    let fetched = false;
    const realFetch = sandbox.fetch;
    sandbox.fetch = function (u) { fetched = true; return realFetch(u); };
    windowStub.fetch = sandbox.fetch;
    poll();
    check('poll tick actually refreshes', fetched);
    sandbox.fetch = realFetch;
    windowStub.fetch = realFetch;

    // A tick landing while a previous load is still in flight must not fire
    // a second overlapping request against the same shared cursor.
    F.S.fleet.loading = true;
    let reentered = false;
    sandbox.fetch = function (u) { reentered = true; return realFetch(u); };
    windowStub.fetch = sandbox.fetch;
    poll();
    check('poll skips while a load is in flight', reentered === false);
    sandbox.fetch = realFetch;
    windowStub.fetch = realFetch;
    F.S.fleet.loading = false;

    // And it must stay off other pages entirely.
    F.S.page = 'overview';
    let polledElsewhere = false;
    sandbox.fetch = function (u) { polledElsewhere = true; return realFetch(u); };
    windowStub.fetch = sandbox.fetch;
    poll();
    check('poll does nothing when another page is open', polledElsewhere === false);
    sandbox.fetch = realFetch;
    windowStub.fetch = realFetch;
    F.S.page = 'fleet';
  }

  // 9. The zero-state placeholder is removed once traces arrive.
  //
  // reconcileList only owns nodes carrying [data-key], so a placeholder
  // written straight into innerHTML was invisible to it and survived every
  // later render — and because rows insert before firstChild, live traces
  // rendered *above* a stale "no traces yet" message.
  console.log('\n[9] zero-state clears when traces arrive');
  {
    const body = new El('div');
    registry['#fleet-traces'] = body;

    F.renderFleetTraces([]);
    const empties = body.querySelectorAll('.fleet-empty');
    eq('empty fleet shows one placeholder', empties.length, 1);

    F.renderFleetTraces([]);
    eq('re-rendering empty does not duplicate it', body.querySelectorAll('.fleet-empty').length, 1);

    const traces = [
      { trace_id: 'aaaa1111aaaa1111aaaa1111aaaa1111', services: ['gateway', 'auth-service'], duration_ms: 12, status: 200, span_count: 2, start_ns: 1e6 },
      { trace_id: 'bbbb2222bbbb2222bbbb2222bbbb2222', services: ['gateway', 'orders-service'], duration_ms: 30, status: 200, span_count: 3, start_ns: 2e6 }
    ];
    F.renderFleetTraces(traces);
    eq('placeholder gone once traces exist', body.querySelectorAll('.fleet-empty').length, 0);
    eq('both traces rendered', body.querySelectorAll('[data-key]').length, 2);

    // Draining back to zero restores the placeholder rather than leaving
    // the last rows on screen forever.
    F.renderFleetTraces([]);
    eq('rows removed when the fleet drains', body.querySelectorAll('[data-key]').length, 0);
    eq('placeholder returns', body.querySelectorAll('.fleet-empty').length, 1);
  }

  // 10. Every observed edge is a mesh: a request arc AND a response arc.
  //
  // The graph used to draw one arrow per edge, which showed that A calls B
  // but never that B answered — so a failed hop had nothing to colour red,
  // and mid-playback there was no way to tell an outstanding call from one
  // that had already returned. Both legs are asserted by their endpoints,
  // because "two lines were drawn" is also true of a duplicate.
  console.log('\n[10] each edge draws both a request and a response leg');
  {
    const topo = makeTopology();
    F.S.fleet.playback = null;
    F.S.fleet.topology = topo;
    canvas.draws.reset();
    F.renderFleetTopology(topo, null, null);

    const pos = F.fleetTopoLayout(topo.nodes, topo.edges, 'gateway', 900, 340);
    const arcs = canvas.draws.arcs();
    // Endpoints are floats, so matching is by rounded coordinate pair.
    function at(p) { return Math.round(p.x) + ',' + Math.round(p.y); }
    const drawn = new Set(arcs.map(function (a) {
      return (a.from ? at(a.from) : '?') + '->' + (a.to ? at(a.to) : '?');
    }));

    let bothLegs = 0;
    topo.edges.forEach(function (e) {
      const a = pos[e.caller], b = pos[e.callee];
      const fwd = drawn.has(at(a) + '->' + at(b));
      const back = drawn.has(at(b) + '->' + at(a));
      check('request leg ' + e.caller + '->' + e.callee, fwd);
      check('response leg ' + e.callee + '->' + e.caller, back);
      if (fwd && back) bothLegs++;
    });
    eq('all four edges drew both legs', bothLegs, topo.edges.length);

    // The user's own call and its answer are part of the mesh too.
    const u = pos[F.FLEET_USER_KEY], g = pos['gateway'];
    check('user request leg drawn', drawn.has(at(u) + '->' + at(g)));
    check('user response leg drawn', drawn.has(at(g) + '->' + at(u)));

    // Two legs per service edge, plus the user's pair.
    eq('arc count is exactly two per edge plus the user pair',
      arcs.length, topo.edges.length * 2 + 2);
  }

  // 11. Per-hop timing is printed on the graph, not left to a hover.
  //
  // Aggregate percentiles show when no trace is open; the moment one is,
  // the labels switch to that trace's own measured hop durations — which is
  // the number a reader actually wants while stepping through an incident.
  console.log('\n[11] hop timings are labelled inline and follow the open trace');
  {
    const topo = makeTopology();
    F.S.fleet.playback = null;
    F.S.fleet.topology = topo;
    canvas.draws.reset();
    F.renderFleetTopology(topo, null, null);
    const agg = canvas.draws.edgeLabels();
    eq('one label per edge with no trace open', agg.length, topo.edges.length);
    check('aggregate labels show percentiles', agg.every(function (t) { return /^p50 /.test(t); }),
      JSON.stringify(agg));
    check('the p50 value is the real edge stat', agg.indexOf('p50 5.0ms · p95 9.0ms') >= 0,
      JSON.stringify(agg));

    // With a trace open, labels become that trace's measured hop durations.
    const ui = await buildRow();
    canvas.draws.reset();
    F.renderFleetTopology(F.S.fleet.topology, F.S.fleet.services, pb());
    const hopLabels = canvas.draws.edgeLabels();
    check('measured hops replace the aggregates',
      hopLabels.every(function (t) { return !/^p50 /.test(t); }), JSON.stringify(hopLabels));
    // Every child span in the fixture runs 100ms.
    check('a measured hop reads as its span duration', hopLabels.indexOf('100.0ms') >= 0,
      JSON.stringify(hopLabels));

    // And the timings themselves are derived from the tree, not guessed.
    const hops = F.fleetHopTimings(pb());
    const authHop = hops['gateway\u0000auth-service'];
    check('gateway->auth hop recovered from the tree', !!authHop);
    eq('hop carries its span duration', authHop.ms, 100);
    eq('hop counted one call', authHop.calls, 1);
    check('orders->billing hop recovered', !!hops['orders-service\u0000billing-service']);
    // The root span has no caller, so it is nobody's hop.
    check('root span produces no hop', hops['\u0000gateway'] === undefined);

    // A hop is in flight while the cursor sits on it and returned after,
    // which is what drives the two legs' colours.
    check('flatten stamps the calling service',
      pb().spans[1]._callerService === 'gateway',
      'got ' + JSON.stringify(pb().spans[1]._callerService));

    // Stepping must not throw with a cursor mid-trace.
    ui.controls.next.dispatch('click');
    ui.controls.next.dispatch('click');
    let threw = null;
    try { F.renderFleetTopology(F.S.fleet.topology, F.S.fleet.services, pb()); } catch (e) { threw = e; }
    check('renders mid-trace without throwing', threw === null, threw && threw.message);
  }

  console.log('');
  if (failures.length) {

    console.log('FAILURES (' + failures.length + '):');

    failures.forEach(function (f) { console.log('  - ' + f); });
    process.exit(1);
  }
  console.log('PASS');
}

main().catch(function (e) {
  console.error('harness error: ' + (e && e.stack || e));
  process.exit(1);
});
