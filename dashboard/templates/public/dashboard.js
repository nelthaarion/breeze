// Breeze Dashboard Client Runtime
// Works WITH Breeze's SPA runtime — uses breeze.fetch/breeze.poll where
// appropriate, and regular fetch for JSON API calls.
(function(){
'use strict';
if(window.BreezeDash) return;

var S = {
  base: '',
  ws: null,
  wsConnected: false,
  snapshot: null,
  history: [],
  page: '',
  requests: [],
  queries: [],
  timelines: [],
  logs: {app:[], http:[], error:[], panic:[], warning:[]},
  logTab: 'app',
  routes: [],
  events: [],
  eventsMeta: null,
  eventFilter: {q:'', failedOnly:false},
  eventSel: null,
  apiRoutes: [],

  apiRouteSel: null,
  apiResp: null,
  apiSnippetLang: 'curl',
  reqFilter: {method:'', status:'', route:'', user:''},
  qSearch: '',
  logSearch: '',
  _slowOnly: false,
  _routeSearch: '',
  caps: {slow_request_ms: 500, slow_query_ms: 100},
  reqPaused: false,
  reqBuffer: [],
  _fresh: {},
  apiForm: {},
  apiFilter: '',
  dbTable: null,
  dbState: {page:1, pageSize:50, search:''},
  fleet: {cursor:'', hasMore:false, loading:false, traces:[], topology:null, services:[], incidents:[], playback:null},
};

// ─── Utils ─────────────────────────────────────────────────────────────
function $(sel, root){return (root||document).querySelector(sel);}
function $$(sel, root){return Array.from((root||document).querySelectorAll(sel));}
function el(tag, attrs, kids){
  var e = document.createElement(tag);
  if(attrs) for(var k in attrs){
    if(k==='class') e.className = attrs[k];
    else if(k==='html') e.innerHTML = attrs[k];
    else if(k==='text') e.textContent = attrs[k];
    else if(k.startsWith('on') && typeof attrs[k]==='function') e.addEventListener(k.slice(2), attrs[k]);
    else e.setAttribute(k, attrs[k]);
  }
  if(kids) (Array.isArray(kids)?kids:[kids]).forEach(function(k){
    if(k==null) return;
    e.appendChild(typeof k==='string'?document.createTextNode(k):k);
  });
  return e;
}
// ─── Motion, safety and feedback helpers ───────────────────────────────
// Every animation in this file asks motionOK() first: people who ask their OS for
// reduced motion get the same dashboard, with the state changes and none of the movement.
var _reducedMotion = null;
try { _reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)'); } catch(e){}
function motionOK(){ return !(_reducedMotion && _reducedMotion.matches); }

// cls makes a server-supplied word safe to use as a CSS class name or inside a class attribute.
function cls(s){ return String(s==null?'':s).replace(/[^A-Za-z0-9_-]/g, ''); }
// attr makes a value safe inside a double-quoted HTML attribute.
function attr(s){ return escapeHTML(s); }
// num coerces to a finite number for places that print one unescaped.
function num(n){ n = Number(n); return isFinite(n) ? n : 0; }
// healthState maps whatever a probe reported onto the three states the page draws.
function healthState(s){
  s = String(s||'').toLowerCase();
  return (s==='green'||s==='yellow'||s==='red') ? s : 'yellow';
}
// connState maps a connection status onto the classes the Architecture page styles.
function connState(s){
  s = String(s||'').toLowerCase();
  return (s==='connected'||s==='disconnected'||s==='degraded') ? s : 'unknown';
}
function isZeroTime(d){ return d.getUTCFullYear() <= 1 || d.getTime() <= 0; }

// rafBatch coalesces repeated render requests into one per animation frame. A live
// batch can hold hundreds of events, and each one used to rebuild its whole table.
var _batched = {};
function rafBatch(name, fn){
  if(_batched[name]) return;
  var run = function(){ _batched[name] = 0; try { fn(); } catch(e){ if(window.console) console.error(e); } };
  _batched[name] = (window.requestAnimationFrame ? requestAnimationFrame(run) : setTimeout(run, 16));
}

// toast shows a short message that does not block the page (the explorer used alert()).
function toast(message, kind, ms){
  var host = document.getElementById('toasts');
  if(!host){
    host = el('div', {id:'toasts', class:'toasts', 'aria-live':'polite', 'aria-atomic':'false'});
    document.body.appendChild(host);
  }
  var t = el('div', {class:'toast '+(kind||'info'), role: kind==='error' ? 'alert' : 'status'}, [
    el('span', {class:'toast-dot', 'aria-hidden':'true'}),
    el('span', {class:'toast-msg', text:String(message)})
  ]);
  host.appendChild(t);
  requestAnimationFrame(function(){ t.classList.add('in'); });
  var closed = false;
  function close(){
    if(closed) return; closed = true;
    t.classList.remove('in'); t.classList.add('out');
    setTimeout(function(){ if(t.parentNode) t.parentNode.removeChild(t); }, motionOK() ? 240 : 0);
  }
  var timer = setTimeout(close, ms || (kind==='error' ? 6000 : 3600));
  t.addEventListener('click', function(){ clearTimeout(timer); close(); });
  while(host.children.length > 4) host.removeChild(host.firstChild);
  return close;
}
var _noticeAt = {};
// notifyError is toast('error') at most once per message every few seconds, so a page that
// polls a failing endpoint does not bury the screen.
function notifyError(message){
  var now = Date.now();
  if(_noticeAt[message] && now - _noticeAt[message] < 8000) return;
  _noticeAt[message] = now;
  toast(message, 'error');
}

// tween moves a number from a to b over ms, calling step(value) each frame.
function tween(a, b, ms, step, done){
  if(!motionOK() || !isFinite(a) || !isFinite(b) || a === b){ step(b); if(done) done(); return function(){}; }
  var t0 = null, raf = 0, dead = false;
  function frame(ts){
    if(dead) return;
    if(t0 === null) t0 = ts;
    var p = Math.min(1, (ts - t0) / ms);
    var e = 1 - Math.pow(1 - p, 3); // ease-out cubic
    step(a + (b - a) * e);
    if(p < 1) raf = requestAnimationFrame(frame); else if(done) done();
  }
  raf = requestAnimationFrame(frame);
  return function(){ dead = true; if(raf) cancelAnimationFrame(raf); };
}

// reconcileList patches a container's children to match `items`, keyed by
// keyFn(item), instead of replacing innerHTML wholesale. This is what most
// render*() functions in this file don't do — they rebuild their whole
// subtree on every refresh, which throws away any per-row UI state (an open
// accordion, scroll position inside a long table, focus) even when the
// underlying data for that row hasn't changed.
//
//   container   — element whose children are the rendered rows
//   items       — array of data objects
//   keyFn       — item -> stable string key (e.g. trace_id)
//   sigFn       — item -> string signature; if unchanged since last render,
//                 the existing DOM node for that key is left completely
//                 untouched (so any state a user toggled on it survives)
//   buildFn     — (item) -> new DOM node for a key seen for the first time
//   updateFn    — (existingNode, item) -> mutate node in place when sigFn
//                 changed but the node itself should be kept (not replaced),
//                 so any accordion-open class the user set on it survives
//
// Existing nodes are matched by a data-key attribute the caller's buildFn
// must set. Nodes for keys no longer present in `items` are removed; nodes
// for new keys are inserted in the correct position; everything else is
// left alone or patched via updateFn, never replaced outright.
function reconcileList(container, items, keyFn, sigFn, buildFn, updateFn){
  if(!container) return;
  var existing = {};
  $$('[data-key]', container).forEach(function(node){ existing[node.dataset.key] = node; });

  var seen = {};
  var prevNode = null;
  items.forEach(function(item){
    var key = String(keyFn(item));
    seen[key] = true;
    var sig = sigFn ? String(sigFn(item)) : null;
    var node = existing[key];

    if(!node){
      node = buildFn(item);
      node.dataset.key = key;
      if(sig!=null) node.dataset.sig = sig;
    } else if(sig!=null && node.dataset.sig !== sig){
      if(updateFn) updateFn(node, item);
      node.dataset.sig = sig;
    }
    // else: identical signature, existing node left completely untouched —
    // this is what preserves an open accordion / scroll offset across a
    // refresh that didn't actually change this row's data.

    // Ensure correct order without detaching nodes that are already in
    // place (which would otherwise reset e.g. an open <details> or CSS
    // transition state even for an untouched node).
    if(prevNode){
      if(prevNode.nextSibling !== node) container.insertBefore(node, prevNode.nextSibling);
    } else if(container.firstChild !== node){
      container.insertBefore(node, container.firstChild);
    }
    prevNode = node;
  });

  Object.keys(existing).forEach(function(key){
    if(!seen[key]) existing[key].remove();
  });
}
function fmtTime(t){
  if(!t) return '-';
  var d = new Date(t);
  if(isNaN(d)) return t;
  if(isZeroTime(d)) return '-';
  return d.toLocaleTimeString([], {hour12:false}) + '.' + String(d.getMilliseconds()).padStart(3,'0');
}
function fmtDate(t){
  if(!t) return '-';
  var d = new Date(t);
  if(isNaN(d)) return t;
  if(isZeroTime(d)) return '-';
  return d.toISOString().slice(0,19).replace('T',' ');
}
function fmtBytes(n){
  if(n==null) return '-';
  if(n<1024) return n+' B';
  if(n<1024*1024) return (n/1024).toFixed(1)+' KB';
  if(n<1024*1024*1024) return (n/1024/1024).toFixed(1)+' MB';
  return (n/1024/1024/1024).toFixed(2)+' GB';
}
function fmtNum(n, digits){
  if(n==null) return '-';
  if(typeof n!=='number') n = Number(n);
  if(isNaN(n)) return '-';
  return n.toFixed(digits||0);
}
function fmtDur(us){
  if(us==null) return '-';
  if(us<1000) return us+'\u00b5s';
  if(us<1000000) return (us/1000).toFixed(2)+'ms';
  return (us/1000000).toFixed(3)+'s';
}
function fmtMS(ms){
  if(ms==null) return '-';
  if(ms<1) return ms.toFixed(2)+'ms';
  if(ms<1000) return ms.toFixed(1)+'ms';
  return (ms/1000).toFixed(2)+'s';
}
function statusClass(s){
  if(s>=200 && s<300) return 's2';
  if(s>=300 && s<400) return 's3';
  if(s>=400 && s<500) return 's4';
  return 's5';
}
function escapeHTML(s){
  if(s==null) return '';
  return String(s).replace(/[&<>"']/g, function(c){
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];
  });
}

// ─── API client ────────────────────────────────────────────────────────
function throwIfAborted(signal){
  if(!signal || !signal.aborted) return;
  var error = new Error('Request aborted');
  error.name = 'AbortError';
  throw error;
}
// loginURL sends a signed-out person to the login page and brings them back to where they were.
function loginURL(){
  return S.base + '/login?next=' + encodeURIComponent(location.pathname + location.search);
}
function failure(r, data){
  var e = new Error((data && data.error) ? data.error : ('HTTP ' + r.status));
  e.status = r.status;
  e.data = data;
  return e;
}
function api(path, options){
  options = options || {};
  var signal = options.signal;
  return fetch(S.base + '/api/' + path, options).then(function(r){
    // Fetch normally rejects an aborted request, but an abort can race with a
    // response that has already fulfilled. Check both sides of JSON parsing
    // so a stale page response can never mutate the next page's shared state.
    throwIfAborted(signal);
    if(r.status === 401){
      // Session expired — redirect to login.
      window.location.href = loginURL();
      throw new Error('unauthorized');
    }
    if(!r.ok){
      // A failed load used to be swallowed by every page's catch, which then drew its
      // empty state ("No requests yet") as if the server had answered with nothing.
      return r.json().catch(function(){ return null; }).then(function(d){
        var e = failure(r, d);
        notifyError('Could not load ' + path.split('?')[0] + ': ' + e.message);
        throw e;
      });
    }
    return r.json().then(function(data){
      throwIfAborted(signal);
      return data;
    });
  }, function(err){
    if(!err || err.name !== 'AbortError') notifyError('Cannot reach the dashboard server');
    throw err;
  });
}
// apiPost rejects on a non-2xx answer, with the server's message on the error. Its callers
// say what they were doing; the generic toast api() raises would not.
function apiPost(path, body, options){
  options = options || {};
  var signal = options.signal;
  return fetch(S.base + '/api/' + path, {
    method: 'POST',
    headers: Object.assign({'Content-Type': 'application/json'}, options.headers || {}),
    body: JSON.stringify(body||{}),
    signal: signal
  }).then(function(r){
    throwIfAborted(signal);
    if(r.status === 401){
      window.location.href = loginURL();
      throw new Error('unauthorized');
    }
    return r.json().catch(function(){ return null; }).then(function(data){
      throwIfAborted(signal);
      if(!r.ok) throw failure(r, data);
      return data;
    });
  }, function(err){
    if(!err || err.name !== 'AbortError') notifyError('Cannot reach the dashboard server');
    throw err;
  });
}

// ─── WebSocket ─────────────────────────────────────────────────────────
// Reconnection backs off (1s, 2s, 4s … 30s, with jitter) instead of retrying every two
// seconds forever, and a socket that never opens is usually an expired session, so the
// API is asked: its 401 sends the person to the login page.
var _wsTries = 0, _wsTimer = 0;
function scheduleWS(){
  clearTimeout(_wsTimer);
  var base = Math.min(30000, 1000 * Math.pow(2, Math.min(_wsTries, 5)));
  _wsTimer = setTimeout(connectWS, base * (0.75 + Math.random() * 0.5));
  _wsTries++;
}
function checkSession(){
  fetch(S.base + '/api/capabilities').then(function(r){
    if(r.status === 401) window.location.href = loginURL();
  }).catch(function(){});
}
function connectWS(){
  var proto = location.protocol==='https:'?'wss:':'ws:';
  var url = proto + '//' + location.host + S.base + '/ws';
  var opened = false;
  try {
    S.ws = new WebSocket(url);
  } catch(e){
    scheduleWS();
    return;
  }
  S.ws.onopen = function(){
    opened = true;
    _wsTries = 0;
    S.wsConnected = true;
    updateWSIndicator();
  };
  S.ws.onclose = function(){
    S.wsConnected = false;
    updateWSIndicator();
    if(!opened && _wsTries >= 1) checkSession();
    scheduleWS();
  };
  S.ws.onerror = function(){try{S.ws.close();}catch(e){}};
  S.ws.onmessage = function(ev){
    var msg;
    try { msg = JSON.parse(ev.data); } catch(e){ return; }
    if(msg.type==='snapshot'){
      S.snapshot = msg;
      var clock = $('#server-time');
      if(clock && msg.time){
        // The topbar's clock element was never filled. It now shows the time of the latest update, which
        // doubles as a heartbeat: it stops moving when the stream does.
        var when = new Date(msg.time);
        if(!isNaN(when)) clock.textContent = when.toLocaleTimeString([], {hour12:false});
      }
      if(S.history.length>120) S.history.shift();
      S.history.push(msg.metrics);
      if(S.page==='overview') rafBatch('overview', renderOverview);
    } else if(msg.type==='batch'){
      // The hub batches live records into one frame every 100ms. Each
      // entry carries its own channel, so unwrap and dispatch each one.
      if(msg.events && msg.events.length){
        for(var i=0;i<msg.events.length;i++){
          onEvent(msg.events[i].channel, msg.events[i].data);
        }
      }
      if(msg.dropped > 0) notifyInfo('The live stream skipped ' + msg.dropped + ' events: traffic is too fast to show every request.');
    } else if(msg.type==='event'){
      onEvent(msg.channel, msg.data);
    }
  };

}
var _infoAt = 0;
function notifyInfo(message){
  var now = Date.now();
  if(now - _infoAt < 10000) return;
  _infoAt = now;
  toast(message, 'info');
}
function updateWSIndicator(){
  var dot = $('#ws-dot');
  if(dot) dot.className = 'ws-dot ' + (S.wsConnected?'on':'off');
  var txt = $('#ws-status');
  if(txt) txt.textContent = S.wsConnected?'live':'reconnecting...';
  var box = $('#ws-box');
  if(box) box.setAttribute('data-state', S.wsConnected?'live':'down');
}

// ─── Event dispatch ────────────────────────────────────────────────────
// Renders are coalesced to one per animation frame: a batch can carry hundreds of events and each
// used to rebuild its whole table before the next one was even applied.
function onEvent(ch, data){
  if(ch==='request'){
    if(S.reqPaused){
      S.reqBuffer.push(data);
      if(S.reqBuffer.length>500) S.reqBuffer.shift();
      if(S.page==='requests') rafBatch('reqpaused', updateReqPaused);
      return;
    }
    S.requests.push(data);
    if(S.requests.length>500) S.requests.shift();
    if(data && data.id) S._fresh[data.id] = true;
    if(S.page==='requests') rafBatch('requests', renderRequests);
  } else if(ch==='query'){
    S.queries.push(data);
    if(S.queries.length>300) S.queries.shift();
    if(S.page==='queries') rafBatch('queries', renderQueries);
  } else if(ch==='timeline'){
    S.timelines.unshift(data);
    if(S.timelines.length>50) S.timelines.pop();
    if(S.page==='timeline') rafBatch('timelines', renderTimelineList);
  } else if(ch==='event'){
    if(data && data.query){
      var q=data.query;
      S.queries.push({
        id:q.id,time:q.time,sql:q.sql,duration_us:q.duration_us,
        duration_ms:(q.duration_us||0)/1000,rows:q.rows,file:q.file,line:q.line,
        slow:!!q.slow,error:q.error||''
      });
      if(S.queries.length>300) S.queries.shift();
      if(S.page==='queries') rafBatch('queries', renderQueries);
    }
    S.events.unshift(data);
    if(S.events.length>500) S.events.pop();
    if(S.page==='events') rafBatch('events', renderEvents);
  }
}


// Page-owned event listeners share the page's AbortSignal with its initial
// API loads. Leaving/re-entering a page therefore removes all view listeners
// and cancels its outstanding reads with one lifecycle operation. A WeakMap
// replaces same-node/same-event handlers during a re-init without retaining
// detached DOM nodes.
var _pageEventMap = new WeakMap();
function pageOn(node, type, handler, options){
  if(!node || typeof node.addEventListener !== 'function' || typeof handler !== 'function') return;
  var handlers = _pageEventMap.get(node);
  if(!handlers){ handlers = new Map(); _pageEventMap.set(node, handlers); }
  var previous = handlers.get(type);
  if(previous) node.removeEventListener(type, previous.handler, previous.options);
  var opts = typeof options === 'boolean' ? {capture:options} : Object.assign({}, options || {});
  if(_activePageController && !_activePageController.signal.aborted) opts.signal = _activePageController.signal;
  node.addEventListener(type, handler, opts);
  handlers.set(type, {handler:handler, options:opts});
}

// Page-owned polling: one active timer and one in-flight request per page.
// A fixed-delay loop is used instead of setInterval, so a slow API response
// cannot overlap the next tick. Navigation aborts the outstanding request.
var _activePagePoll = null;
var _activePageController = null;
function stopPageWork(){
  if(_activePageController){ _activePageController.abort(); _activePageController=null; }
  stopPagePoll();
  // Timers and pointer state belong to their view even when they are not API
  // pollers. Leaving Fleet must stop trace playback; leaving Architecture must
  // not carry an in-progress drag into a later visit.
  var playback = S.fleet && S.fleet.playback;
  if(playback){
    if(playback.timer) clearTimeout(playback.timer);
    playback.timer = null;
    playback.running = false;
    playback.finishedAt = 0;
  }
  // Fleet's canvas animation is driven by a shared requestAnimationFrame
  // loop rather than the playback timeout. Cancel its pending frame as well;
  // otherwise a paused 0.0x trace or the post-playback fade can keep the loop
  // alive after the Fleet view has been replaced.
  if(typeof _fleetRaf !== 'undefined' && _fleetRaf){
    if(typeof cancelAnimationFrame === 'function') cancelAnimationFrame(_fleetRaf);
    _fleetRaf = 0;
  }
  if(typeof archDragging !== 'undefined') archDragging = false;
}
function stopPagePoll(){
  var poll = _activePagePoll;
  if(!poll) return;
  _activePagePoll = null;
  poll.stopped = true;
  if(poll.timer) clearTimeout(poll.timer);
  if(poll.controller) poll.controller.abort();
}
function startPagePoll(page, fn, intervalMs){
  stopPagePoll();
  var poll = {page:page, timer:0, controller:null, stopped:false};
  _activePagePoll = poll;
  function schedule(){
    if(!poll.stopped) poll.timer = setTimeout(run, intervalMs);
  }
  function run(){
    poll.timer = 0;
    if(poll.stopped) return;
    if(S.page !== page){ schedule(); return; }
    var controller = new AbortController();
    poll.controller = controller;
    var result;
    try { result = fn(controller.signal); }
    catch(e) { result = Promise.reject(e); }
    Promise.resolve(result).catch(function(e){
      // api() has already told the person about a failed load; a poll just tries again.
      if(!e || e.name !== 'AbortError') return;
    }).finally(function(){
      if(poll.controller === controller) poll.controller = null;
      if(!poll.stopped) schedule();
    });
  }
  schedule();
}

// ─── Page initialization ───────────────────────────────────────────────
var PAGES = {
  overview: {title:'Overview', init: initOverview},
  routes: {title:'Routes', init: initRoutes},
  api: {title:'API Explorer', init: initAPI},
  requests: {title:'Live Requests', init: initRequests},
  queries: {title:'Database Queries', init: initQueries},
  cache: {title:'Cache', init: initCache},
  logs: {title:'Logs', init: initLogs},
  health: {title:'Health', init: initHealth},
  performance: {title:'Performance', init: initPerformance},
  timeline: {title:'Timeline', init: initTimeline},
  architecture: {title:'Architecture', init: initArchitecture},
  events: {title:'Events', init: initEvents},
  video: {title:'Video Streaming', init: initVideo},
  database: {title:'Database Browser', init: initDatabase},
  queue: {title:'Queue', init: initQueue},
  scheduler: {title:'Scheduler', init: initScheduler},
  fleet: {title:'Fleet View', init: initFleet},
};



function currentPageSignal(){
  return _activePageController ? _activePageController.signal : undefined;
}
function initPage(page){
  stopPageWork();
  _activePageController = new AbortController();
  S.page = page;
  var titleEl = $('#page-title');
  if(titleEl && PAGES[page]) titleEl.textContent = PAGES[page].title;
  // The tab's own title followed the page only on a full load.
  if(PAGES[page]) document.title = PAGES[page].title + ' \u00b7 Breeze Dashboard';
  // Update nav active state — this runs on every SPA navigation.
  $$('.nav-item').forEach(function(n){
    var on = n.getAttribute('data-nav') === page;
    n.classList.toggle('active', on);
    if(on) n.setAttribute('aria-current', 'page'); else n.removeAttribute('aria-current');
  });
  closeNavDrawer();
  var app = $('#breeze-app');
  if(app && app.classList && motionOK()){
    // The content area eases in on each navigation; the shell does not move.
    app.classList.remove('page-enter');
    void app.offsetWidth;
    app.classList.add('page-enter');
  }
  if(PAGES[page] && PAGES[page].init) PAGES[page].init(_activePageController.signal);
}

// ─── Fleet View ─────────────────────────────────────────────────────────
function initFleet(signal){
  var tag=$('#fleet-tag');
  if(tag) pageOn(tag, 'change', function(){ loadFleet(false, currentPageSignal()); });
  var next=$('#fleet-next-page');
  if(next) pageOn(next, 'click', function(){ if(S.fleet.hasMore && !S.fleet.loading) loadFleet(true, currentPageSignal()); });
  S.fleet.cursor=''; S.fleet.traces=[];
  loadFleet(false, signal);
  // Polling is owned by the page lifecycle. Fleet refreshes itself without
  // overlapping requests and stops cleanly on navigation.
  // Fleet View previously had no refresh at all —
  // it only ever updated on the tag-filter change event. That's now safe to
  // add because renderFleetTraces/renderFleetTopology reconcile in place
  // instead of rebuilding the DOM, so a poll tick no longer collapses an
  // open accordion or resets scroll position.
  //
  // Exactly one poller is active for this page visit. startPagePoll stops the
  // previous page's timer/request before arming this one, so navigation back
  // to Fleet cannot leave old pollers behind. Previously, setInterval was
  // added on every visit and never cleared. By the fifth
  // visit five pollers were interleaving calls to loadFleet(), each mutating
  // the shared S.fleet.cursor/traces mid-flight: one tick's response would
  // land on a cursor a different tick had already advanced, so pages of
  // traces overwrote each other and the list stopped tracking what the
  // aggregator actually held. It looked like Fleet View only refreshed on a
  // tab change, because a tab change re-ran the one code path that reset
  // cursor and traces from scratch (above) before any poller could interfere.
  startPagePoll('fleet', function(signal){
    return !S.fleet.loading ? loadFleet(false, signal, true) : Promise.resolve();
  }, 5000);
}

var _fleetLoadController = null;
function loadFleet(nextPage, signal, refreshOnly){
  // A filter change or explicit reload supersedes the previous Fleet load.
  // Keep the request controller separate from the page controller so both a
  // new load and leaving the page can cancel all four API requests together.
  if(_fleetLoadController) _fleetLoadController.abort();
  var controller = new AbortController();
  _fleetLoadController = controller;
  var parentAbort = null;
  if(signal){
    if(signal.aborted) controller.abort();
    else {
      parentAbort = function(){controller.abort();};
      signal.addEventListener('abort', parentAbort, {once:true});
    }
  }
  var tag=$('#fleet-tag'), suffix='page_size=40&min_services=2';
  if(tag && tag.value) suffix+='&tag='+encodeURIComponent(tag.value);
  if(nextPage && S.fleet.cursor) suffix+='&cursor='+encodeURIComponent(S.fleet.cursor);
  else if(!nextPage && !refreshOnly) S.fleet.cursor='';
  S.fleet.loading=true;
  var requestSignal = controller.signal;
  var requests = [
    api('fleet/services', {signal:requestSignal}),
    api('fleet/topology', {signal:requestSignal})
  ];
  if(!refreshOnly) requests.push(api('fleet/traces?'+suffix, {signal:requestSignal}));
  requests.push(api('fleet/incidents', {signal:requestSignal}));
  return Promise.all(requests).then(function(v){
    if(requestSignal.aborted || _fleetLoadController !== controller) return;
    var dead=$('#fleet-unreachable'); if(dead) dead.style.display='none';
    S.fleet.services=v[0]||[]; S.fleet.topology=v[1]||{};
    S.fleet.incidents=v[refreshOnly?2:3]||[];
    if(!refreshOnly){
      var page=Array.isArray(v[2])?{traces:v[2],has_more:false}:v[2]||{};
      S.fleet.hasMore=!!page.has_more; S.fleet.cursor=page.next_cursor||'';
      if(nextPage) S.fleet.traces=S.fleet.traces.concat(page.traces||[]); else S.fleet.traces=page.traces||[];
    }
    renderFleetCards(S.fleet.services,S.fleet.incidents); renderFleetTopology(S.fleet.topology,S.fleet.services); renderFleetTraces(S.fleet.traces);
    var status=$('#fleet-page-status'); if(status) status.textContent=S.fleet.traces.length+' traces loaded';
    var next=$('#fleet-next-page'); if(next) next.style.display=S.fleet.hasMore?'inline-flex':'none';
  }).catch(function(error){
    if(error && error.name === 'AbortError') return;
    if(_fleetLoadController !== controller) return;
    var dead=$('#fleet-unreachable');
    if(dead){dead.style.display='block';dead.innerHTML='<div class="icon">&#9888;</div>Fleet Aggregator unreachable — check that it is running and the configured URL is correct.';}
  }).finally(function(){
    if(signal && parentAbort) signal.removeEventListener('abort', parentAbort);
    if(_fleetLoadController === controller){_fleetLoadController=null; S.fleet.loading=false;}
  });
}
function renderFleetCards(services,incidents){
  var root=$('#fleet-cards'); if(!root)return;
  services=services||[]; incidents=incidents||[];
  var up=services.filter(function(s){return s.status==='up';}).length;
  root.innerHTML='<div class="card"><div class="label">Services</div><div class="value">'+services.length+'</div></div>'+
    '<div class="card"><div class="label">Healthy</div><div class="value">'+up+'</div></div>'+
    '<div class="card"><div class="label">Active incidents</div><div class="value">'+incidents.length+'</div></div>';
}
// A trace's signature is everything the collapsed summary row shows, plus
// span_count (a decent proxy for "has this trace grown since last poll").
// Two identical signatures back-to-back means reconcileList leaves that
// row's DOM completely alone — including its open/closed accordion state
// and whatever detail markup is already rendered inside it.
function _fleetTraceSig(t){
  return [t.duration_ms, t.status, t.has_error, t.span_count].join('|');
}
function _fleetTraceBuild(t){
  var row = el('div', {class:'accordion-item fleet-trace'});
  var head = el('div', {class:'accordion-head', role:'button', tabindex:'0'});
  head.innerHTML =
    '<span class="fleet-t-time">'+fmtTime(t.start_ns/1000000)+'</span>'+
    '<span class="fleet-t-services">'+escapeHTML((t.services||[]).join(' → '))+'</span>'+
    '<span class="fleet-t-dur">'+fmtMS(t.duration_ms)+'</span>'+
    '<span class="status '+statusClass(t.status||0)+'">'+(t.has_error?'error':escapeHTML(t.status||'-'))+'</span>'+
    '<span class="fleet-t-spans">'+num(t.span_count)+' spans</span>'+
    '<code class="fleet-t-id">'+escapeHTML(t.trace_id.slice(0,12))+'…</code>'+
    '<span class="accordion-caret">&#9656;</span>';
  var body = el('div', {class:'accordion-body', style:'display:none'});
  head.onclick = function(){ toggleFleetTrace(row, t.trace_id); };
  row.appendChild(head);
  row.appendChild(body);
  return row;
}
function _fleetTraceUpdate(row, t){
  // Only the summary head needs to reflect new data (e.g. duration/status
  // changed on a poll); the body — including whether it's expanded and any
  // already-rendered detail/play-state inside it — is left untouched.
  var head = $('.accordion-head', row);
  if(!head) return;
  head.innerHTML =
    '<span class="fleet-t-time">'+fmtTime(t.start_ns/1000000)+'</span>'+
    '<span class="fleet-t-services">'+escapeHTML((t.services||[]).join(' → '))+'</span>'+
    '<span class="fleet-t-dur">'+fmtMS(t.duration_ms)+'</span>'+
    '<span class="status '+statusClass(t.status||0)+'">'+(t.has_error?'error':escapeHTML(t.status||'-'))+'</span>'+
    '<span class="fleet-t-spans">'+num(t.span_count)+' spans</span>'+
    '<code class="fleet-t-id">'+escapeHTML(t.trace_id.slice(0,12))+'…</code>'+
    '<span class="accordion-caret">&#9656;</span>';
  head.onclick = function(){ toggleFleetTrace(row, t.trace_id); };
}
function renderFleetTraces(traces){
  var body=$('#fleet-traces'); if(!body)return; traces=traces||[];
  // The zero-state placeholder is carried on its own node and removed
  // explicitly, because reconcileList can only remove nodes it owns — it
  // matches on [data-key], and the placeholder has none. Left in place it
  // survived every subsequent poll: the first tick on an idle fleet wrote
  // "No multi-service traces yet", and once traces began arriving they were
  // inserted *before* that div (insertBefore(node, firstChild)), so the page
  // showed live rows with a "no traces" message still sitting underneath
  // them. Whether the list looked correct depended on whether the first
  // poll after opening the tab happened to find any traces.
  var placeholder=$('.fleet-empty', body);
  if(traces.length){
    if(placeholder) placeholder.remove();
  } else if(!placeholder){
    body.appendChild(el('div',{class:'empty fleet-empty',text:'No multi-service traces yet'}));
  }
  reconcileList(body, traces, function(t){return t.trace_id;}, _fleetTraceSig, _fleetTraceBuild, _fleetTraceUpdate);
}

// Toggling never touches sibling rows or the page's scroll position — it
// only flips display/class on the one accordion item that was clicked, and
// only fetches detail the first time a given trace is opened (cached on the
// row itself via dataset.loaded) rather than re-fetching and re-rendering
// on every click.
function toggleFleetTrace(row, id){
  var wasOpen = row.classList.contains('open');
  row.classList.toggle('open', !wasOpen);
  var bodyEl = $('.accordion-body', row);
  var caret = $('.accordion-caret', row);
  if(bodyEl) bodyEl.style.display = wasOpen ? 'none' : 'block';
  if(caret) caret.innerHTML = wasOpen ? '&#9656;' : '&#9662;';
  if(wasOpen || !bodyEl || row.dataset.loaded) return;
  row.dataset.loaded = '1';
  api('fleet/traces/'+encodeURIComponent(id), {signal:currentPageSignal()}).then(function(t){
    function stepRow(n,d){
      var s=n||{}, tags=Object.keys(s.tags||{}).map(function(k){return '<span class="badge">'+escapeHTML(k)+'='+escapeHTML(s.tags[k])+'</span>';}).join(' ');
      var html='<div class="t-step" style="margin-left:'+(d*20)+'px"><div><strong>'+escapeHTML(s.service||'unknown')+'</strong> '+escapeHTML(s.method||'')+' '+escapeHTML(s.route||'')+' — '+fmtMS(s.duration_ms||0)+' '+(n.root_cause?'<span class="status s5">ROOT CAUSE</span>':'')+(n.skewed?' &#9888;':'')+'</div><div>'+tags+'</div></div>';
      (n.children||[]).forEach(function(c){html+=stepRow(c,d+1);});
      return html;
    }
    row._fleetTrace=t;
    bodyEl.innerHTML =
      '<div class="fleet-summary">'+escapeHTML(t.summary||'')+'</div>'+
      '<div class="fleet-play-controls">'+
        '<button class="fleet-play-btn" type="button" data-action="play">&#9654; Play</button>'+ 
        '<button type="button" data-action="prev" title="Previous span">&#9664; Prev</button>'+ 
        '<button type="button" data-action="next" title="Next span">Next &#9654;</button>'+ 
        '<label>Speed <select data-action="speed"><option value="1" selected>1x</option><option value="0.5">0.5x</option><option value="0.1">0.1x</option><option value="0.05">0.05x</option><option value="0">0.0x hold</option></select></label>'+ 
      '</div>'+ 
      '<div class="timeline">'+(t.roots||[]).map(function(r){return stepRow(r,0);}).join('')+'</div>';
    $$('.fleet-play-controls [data-action]', bodyEl).forEach(function(control){
      // Buttons report on click, but a <select> only reports a new value on
      // change. Bound to click it handed back whatever was selected BEFORE
      // the user picked, so every speed change applied one step late, and
      // keyboard selection never applied at all.
      var evt = control.tagName === 'SELECT' ? 'change' : 'click';
      pageOn(control, evt, function(ev){ev.stopPropagation(); fleetPlaybackAction(row, t, control.dataset.action, control.value);});
    });
    fleetPlaybackAction(row, t, 'render');
  }).catch(function(error){
    if(error && error.name === 'AbortError') return;
    row.dataset.loaded = '';
    bodyEl.innerHTML = '<div class="empty">Failed to load trace detail</div>';
  });
}
function fleetFlattenTrace(trace){
  var out=[];
  (trace.roots||[]).forEach(function(root){
    (function walk(n,caller){
      if(!n)return;
      // The parent's service, stamped on the span while the tree is still
      // in hand. The topology renderer needs to know which edge a span
      // travelled over, and this is the only walk of the tree — recovering
      // it afterwards would mean re-walking and re-matching spans by an
      // identity the flattened, start-sorted list no longer carries.
      n._callerService=caller||'';
      out.push(n);
      (n.children||[]).forEach(function(kid){walk(kid,n.service);});
    })(root,'');
  });
  return out.sort(function(a,b){return (a.start_ns||0)-(b.start_ns||0);});
}

// Delay before advancing off the span at pb.index, in wall-clock ms.
//
// Speed is a divisor on the span's own duration, so 0.5x runs at half
// speed. 0.0x is the deliberate exception: it means "hold here", reported
// as Infinity so the scheduler simply does not arm a timer. The old code
// computed duration/(pb.speed||1), where 0 is falsy and silently became
// 1x — the one speed the menu could not actually select.
function fleetSpanDelay(pb){
  var span=pb.spans[pb.index]||{};
  var floor=Math.max(80,Number(span.duration_ms||80));
  if(!(pb.speed>0)) return Infinity;
  return floor/pb.speed;
}
// Arms the timer for the next span. Advancing used to recurse through
// fleetPlaybackAction(...,'play'), but 'play' TOGGLES running — so the
// second span switched playback off and it stopped two spans in.
// Scheduling is its own function now, and the toggle stays on the button
// path where a user action actually means "toggle".
function fleetPlaybackSchedule(pb){
  if(pb.timer){clearTimeout(pb.timer);pb.timer=null;}
  if(!pb.running) return;
  var delay=fleetSpanDelay(pb);
  if(!isFinite(delay)) return;          // 0.0x: held, still animating
  pb.timer=setTimeout(function(){
    pb.timer=null;
    if(pb.index>=pb.spans.length-1){
      // End of trace: stop, and mark the moment so the response leg back
      // to the user gets its own visible beat before the graph goes idle.
      pb.running=false; pb.finishedAt=Date.now();
    } else {
      pb.index++;
    }
    renderFleetPlayback(pb);
    fleetPlaybackSchedule(pb);
  }, delay);
}
function fleetPlaybackAction(row, trace, action, value){
  var pb=S.fleet.playback;
  if(!pb || pb.row!==row){
    pb={row:row,trace:trace,spans:fleetFlattenTrace(trace),index:-1,running:false,speed:1,timer:null,finishedAt:0};
    S.fleet.playback=pb;
  }
  if(action==='speed'){
    // Number('0') is 0, a legal speed here, so this guards on NaN rather
    // than falsiness.
    var s=Number(value); pb.speed=isNaN(s)?1:s;
  }
  if(action==='play'){
    pb.running=!pb.running;
    if(pb.running && pb.index>=pb.spans.length-1){pb.index=-1;pb.finishedAt=0;}
  }
  if(action==='next'){pb.running=false;pb.index=Math.min(pb.spans.length-1,pb.index+1);}
  if(action==='prev'){pb.running=false;pb.index=Math.max(-1,pb.index-1);}
  if(pb.running && pb.index<0) pb.index=0;
  if(action==='play'||action==='next'||action==='prev') pb.finishedAt=0;
  renderFleetPlayback(pb);
  fleetPlaybackSchedule(pb);
}
function renderFleetPlayback(pb){
  if(!pb || !pb.row)return;
  $$('.t-step',pb.row).forEach(function(step,i){step.classList.toggle('fleet-step-active', i===pb.index);});
  var button=$('[data-action="play"]',pb.row);
  if(button) button.innerHTML=pb.running?'&#10074;&#10074; Pause':'&#9654; Play';
  renderFleetTopology(S.fleet.topology,S.fleet.services,pb);
}
// ─── Topology drawing ──────────────────────────────────────────────────
//
// Nodes are labeled cards with a role icon, laid out in left-to-right
// layers (User, entry service, then its callees), and the in-flight
// request is drawn as a glow traveling the edge being served. The previous
// version drew unlabeled circles on a ring: with callers and callees at
// arbitrary angles an edge's direction carried no information, longer
// traces crossed over themselves, and there was nowhere sensible to put
// the user. Layering is the gateway-left to leaves-right arrangement the
// fleet spec asks for.

// Role is guessed from the service name, for the icon only. A wrong guess
// costs nothing but a generic box, so this never needs to be exhaustive.
function fleetServiceKind(name){
  var n=String(name||'').toLowerCase();
  if(/gate|edge|proxy|ingress|bff/.test(n)) return 'gateway';
  if(/auth|identity|login|session|token/.test(n)) return 'lock';
  if(/order|cart|checkout|payment|billing|invoice/.test(n)) return 'cart';
  if(/db|database|sql|postgres|mysql|mongo|redis|store|storage/.test(n)) return 'db';
  if(/notif|mail|email|sms|push|alert/.test(n)) return 'bell';
  if(/analytic|report|metric|stats|event/.test(n)) return 'chart';
  return 'box';
}
// Icons are drawn as paths rather than emoji or an icon font: the
// dashboard ships no font files, and emoji rendering differs per platform.
function fleetDrawGlyph(g,kind,cx,cy,color){
  g.save(); g.strokeStyle=color; g.fillStyle=color; g.lineWidth=1.6;
  g.lineJoin='round'; g.lineCap='round';
  if(kind==='user'){
    g.beginPath(); g.arc(cx,cy-4,3.6,0,Math.PI*2); g.stroke();
    g.beginPath(); g.arc(cx,cy+7,6.4,Math.PI*1.15,Math.PI*1.85,true); g.stroke();
  } else if(kind==='gateway'){
    g.beginPath(); g.moveTo(cx-6,cy-6); g.lineTo(cx-6,cy+6); g.stroke();
    g.beginPath(); g.moveTo(cx+6,cy-6); g.lineTo(cx+6,cy+6); g.stroke();
    g.beginPath(); g.moveTo(cx-3,cy); g.lineTo(cx+3,cy); g.stroke();
    g.beginPath(); g.moveTo(cx+1,cy-3); g.lineTo(cx+4,cy); g.lineTo(cx+1,cy+3); g.stroke();
  } else if(kind==='lock'){
    g.beginPath(); g.rect(cx-5,cy-1,10,8); g.stroke();
    g.beginPath(); g.arc(cx,cy-1,3.4,Math.PI,0); g.stroke();
  } else if(kind==='cart'){
    g.beginPath(); g.moveTo(cx-7,cy-5); g.lineTo(cx-4,cy-5); g.lineTo(cx-2,cy+3); g.lineTo(cx+6,cy+3); g.stroke();
    g.beginPath(); g.moveTo(cx-3,cy-2); g.lineTo(cx+7,cy-2); g.stroke();
    g.beginPath(); g.arc(cx-1,cy+6,1.5,0,Math.PI*2); g.fill();
    g.beginPath(); g.arc(cx+5,cy+6,1.5,0,Math.PI*2); g.fill();
  } else if(kind==='db'){
    g.beginPath(); g.ellipse(cx,cy-4,6,2.4,0,0,Math.PI*2); g.stroke();
    g.beginPath(); g.moveTo(cx-6,cy-4); g.lineTo(cx-6,cy+4); g.stroke();
    g.beginPath(); g.moveTo(cx+6,cy-4); g.lineTo(cx+6,cy+4); g.stroke();
    g.beginPath(); g.ellipse(cx,cy+4,6,2.4,0,0,Math.PI); g.stroke();
  } else if(kind==='bell'){
    g.beginPath(); g.moveTo(cx-5,cy+3); g.lineTo(cx+5,cy+3);
    g.lineTo(cx+3,cy-1); g.lineTo(cx+3,cy-3); g.arc(cx,cy-3,3,0,Math.PI,true);
    g.lineTo(cx-3,cy-1); g.closePath(); g.stroke();
    g.beginPath(); g.arc(cx,cy+5,1.6,0,Math.PI); g.stroke();
  } else if(kind==='chart'){
    g.beginPath(); g.moveTo(cx-6,cy+5); g.lineTo(cx-6,cy-1); g.stroke();
    g.beginPath(); g.moveTo(cx-1,cy+5); g.lineTo(cx-1,cy-5); g.stroke();
    g.beginPath(); g.moveTo(cx+4,cy+5); g.lineTo(cx+4,cy+1); g.stroke();
  } else {
    g.beginPath(); g.rect(cx-6,cy-5,12,10); g.stroke();
    g.beginPath(); g.moveTo(cx-6,cy-1); g.lineTo(cx+6,cy-1); g.stroke();
  }
  g.restore();
}
function fleetRoundRect(g,rx,ry,rw,rh,r){
  g.beginPath();
  g.moveTo(rx+r,ry); g.lineTo(rx+rw-r,ry); g.quadraticCurveTo(rx+rw,ry,rx+rw,ry+r);
  g.lineTo(rx+rw,ry+rh-r); g.quadraticCurveTo(rx+rw,ry+rh,rx+rw-r,ry+rh);
  g.lineTo(rx+r,ry+rh); g.quadraticCurveTo(rx,ry+rh,rx,ry+rh-r);
  g.lineTo(rx,ry+r); g.quadraticCurveTo(rx,ry,rx+r,ry);
  g.closePath();
}
// Sentinel key for the user's own node in the position map. Prefixed so a
// service literally named "user" still gets its own distinct node.
var FLEET_USER_KEY='@@breeze-user';
// Depth from the entry service over observed caller-to-callee edges, so a
// service's column is its distance from the request's front door. Nodes no
// edge reaches (reported, but never seen being called in the window) are
// parked in a trailing column rather than dropped.
function fleetTopoLayout(nodes,edges,entry,w,h){
  var depth={}, adj={}, i;
  for(i=0;i<edges.length;i++){
    var from=edges[i].caller||edges[i].from, to=edges[i].callee||edges[i].to;
    if(!from||!to) continue;
    (adj[from]=adj[from]||[]).push(to);
  }
  var queue=[], seen={};
  if(entry){depth[entry]=0;queue.push(entry);seen[entry]=1;}
  var guard=0;
  while(queue.length && guard++ < 4096){
    var cur=queue.shift(), next=adj[cur]||[];
    for(i=0;i<next.length;i++){
      if(seen[next[i]]) continue;      // first visit wins; also breaks cycles
      seen[next[i]]=1; depth[next[i]]=depth[cur]+1; queue.push(next[i]);
    }
  }
  var maxDepth=0, names=nodes.map(function(n){return n.service||n.name;});
  names.forEach(function(n){if(depth[n]>maxDepth)maxDepth=depth[n];});
  names.forEach(function(n){if(depth[n]===undefined)depth[n]=maxDepth+1;});
  var cols={}, colMax=0;
  names.forEach(function(n){var d=depth[n];(cols[d]=cols[d]||[]).push(n);if(d>colMax)colMax=d;});
  // Column 0 is reserved for the user, so services start one column in.
  var span=Math.max(1,colMax+2), colW=w/(span+0.35), pos={};
  Object.keys(cols).forEach(function(d){
    var list=cols[d], cx=colW*(Number(d)+1.55);
    for(var k=0;k<list.length;k++){
      var slot=(k+1)/(list.length+1);
      pos[list[k]]={x:cx,y:h*slot,depth:Number(d)};
    }
  });
  pos[FLEET_USER_KEY]={x:colW*0.55,y:h/2,depth:-1,user:true};
  return pos;
}
// Quadratic control point offset perpendicular to the edge, so the call
// and response legs between one pair of nodes are two visibly separate
// arcs instead of one line drawn over itself.
function fleetCurve(a,b,bow){
  var mx=(a.x+b.x)/2, my=(a.y+b.y)/2, dx=b.x-a.x, dy=b.y-a.y;
  var len=Math.sqrt(dx*dx+dy*dy)||1;
  return {cx:mx+(-dy/len)*bow, cy:my+(dx/len)*bow};
}
function fleetCurvePoint(a,b,c,t){
  var u=1-t;
  return {x:u*u*a.x+2*u*t*c.cx+t*t*b.x, y:u*u*a.y+2*u*t*c.cy+t*t*b.y};
}
function fleetDrawEdge(g,a,b,c,color,width,dash){
  g.save(); g.strokeStyle=color; g.lineWidth=width; g.setLineDash(dash||[]);
  g.beginPath(); g.moveTo(a.x,a.y); g.quadraticCurveTo(c.cx,c.cy,b.x,b.y); g.stroke();
  // Arrowhead along the curve's tangent at the target end.
  var near=fleetCurvePoint(a,b,c,0.93), ang=Math.atan2(b.y-near.y,b.x-near.x);
  g.setLineDash([]); g.beginPath();
  g.moveTo(b.x,b.y);
  g.lineTo(b.x-9*Math.cos(ang-0.38),b.y-9*Math.sin(ang-0.38));
  g.lineTo(b.x-9*Math.cos(ang+0.38),b.y-9*Math.sin(ang+0.38));
  g.closePath(); g.fillStyle=color; g.fill();
  g.restore();
}
// The traveling request: a soft radial blob plus a short bright trail
// behind it, so direction of travel reads at a glance. A single flat dot
// (what this used to draw) gave position but not direction.
function fleetDrawGlow(g,a,b,c,phase,color){
  var p=fleetCurvePoint(a,b,c,phase);
  g.save();
  var grad=g.createRadialGradient(p.x,p.y,0,p.x,p.y,16);
  grad.addColorStop(0,color); grad.addColorStop(0.35,color); grad.addColorStop(1,'rgba(0,0,0,0)');
  g.globalAlpha=0.55; g.fillStyle=grad;
  g.beginPath(); g.arc(p.x,p.y,16,0,Math.PI*2); g.fill();
  g.globalAlpha=1; g.fillStyle=color;
  g.beginPath(); g.arc(p.x,p.y,4.2,0,Math.PI*2); g.fill();
  g.globalAlpha=0.5; g.strokeStyle=color; g.lineWidth=3.2; g.lineCap='round';
  var tail=fleetCurvePoint(a,b,c,Math.max(0,phase-0.12));
  g.beginPath(); g.moveTo(tail.x,tail.y); g.lineTo(p.x,p.y); g.stroke();
  g.restore();
}
// Per-hop timings for the trace currently open, keyed caller→callee.
//
// Rebuilt per render rather than cached: a trace is tens of spans, so the
// walk is free, and a cache would go stale the moment a different trace is
// opened — the exact bug class this file has already been bitten by.
//
// first/last are span positions in playback order. A hop is "in flight"
// while the cursor sits on one of its spans and "returned" once the cursor
// is past the last of them, which is what lets the response leg light up
// without inventing a second timeline.
function fleetHopTimings(playback){
  var out={};
  if(!playback||!playback.spans) return out;
  playback.spans.forEach(function(sp,i){
    var caller=sp._callerService; if(!caller||caller===sp.service) return;
    var key=caller+'\u0000'+sp.service, hop=out[key];
    if(!hop){hop=out[key]={ms:0,calls:0,first:i,last:i,status:sp.status||0};}
    hop.ms+=Number(sp.duration_ms||0);
    hop.calls++;
    if(i<hop.first) hop.first=i;
    if(i>hop.last) hop.last=i;
    // The worst status on the hop wins, so one failed retry is not hidden
    // by a successful one over the same edge.
    if((sp.status||0)>hop.status) hop.status=sp.status||0;
  });
  return out;
}
// Per-hop latency, printed on the arc it belongs to.
//
// The alternatives are a legend or a hover tooltip. A legend makes the
// reader map names back to lines, and canvas has no hover without
// hit-testing every curve. Printed inline it also survives the screenshot
// that gets pasted into an incident channel, which is how these graphs are
// actually shared.
function fleetDrawEdgeLabel(g,a,b,c,text,fill,color){
  if(!text) return;
  var p=fleetCurvePoint(a,b,c,0.5);
  g.save();
  g.font='600 10px system-ui, sans-serif'; g.textAlign='center'; g.textBaseline='middle';
  var bw=g.measureText(text).width+10, bh=15;
  fleetRoundRect(g,p.x-bw/2,p.y-bh/2,bw,bh,7);
  g.globalAlpha=0.94; g.fillStyle=fill; g.fill(); g.globalAlpha=1;
  g.lineWidth=1; g.setLineDash([]); g.strokeStyle=color; g.stroke();
  g.fillStyle=color; g.fillText(text,p.x,p.y);
  g.restore();
}
function fleetDrawNode(g,label,kind,p,fill,accent,textColor,active){

  var bw=124,bh=46,bx=p.x-bw/2,by=p.y-bh/2;
  g.save();
  if(active){g.shadowColor=accent;g.shadowBlur=18;}
  fleetRoundRect(g,bx,by,bw,bh,9); g.fillStyle=fill; g.fill();
  g.shadowBlur=0;
  g.lineWidth=active?2.4:1.4; g.strokeStyle=accent; g.stroke();
  // Left accent bar carries status colour even when the fill is neutral.
  g.save(); fleetRoundRect(g,bx,by,bw,bh,9); g.clip();
  g.fillStyle=accent; g.fillRect(bx,by,4,bh); g.restore();
  fleetDrawGlyph(g,kind,bx+22,p.y,accent);
  g.fillStyle=textColor; g.textAlign='left'; g.textBaseline='middle';
  g.font='600 12px system-ui, sans-serif';
  var full=String(label||''), text=full, max=bw-46;
  while(text.length>4 && g.measureText(text).width>max) text=text.slice(0,-1);
  if(text!==full) text=text+'...';
  g.fillText(text,bx+38,p.y);
  g.restore();
}
// One shared frame loop. Each render used to schedule its own
// requestAnimationFrame, so two overlapping renders (a 5s poll tick
// landing on a playback frame) compounded into two loops that each kept
// scheduling more — the animation visibly sped up and burned CPU the
// longer it played. This module-level handle keeps exactly one frame in
// flight no matter how many callers ask to render.
var _fleetRaf=0;
function fleetTopoNeedsFrame(pb){
  // The animation belongs to Fleet, not to the global dashboard shell. A
  // queued render or a theme change after navigation must never restart it.
  if(S.page !== 'fleet' || !pb) return false;
  if(pb.running) return true;
  if(!(pb.speed>0) && pb.index>=0) return true;   // 0.0x: held, still live
  return !!(pb.finishedAt && Date.now()-pb.finishedAt<1800);
}
function fleetTopoFrame(){
  _fleetRaf=0;
  renderFleetTopology(S.fleet.topology,S.fleet.services,S.fleet.playback);
}
// Colours come from the same CSS variables drawLineChart uses, so the
// graph tracks the active theme rather than the dark-only palette this
// once hardcoded.
function renderFleetTopology(topology,services,playback){
  var c=$('#fleet-topology'); if(!c)return;
  // A poll tick calls this with no playback argument; falling back to the
  // stored one keeps a running animation from being wiped every refresh.
  if(playback===undefined||playback===null) playback=S.fleet.playback;
  var nodes=(topology&&topology.nodes)||[], edges=(topology&&topology.edges)||[];
  if(!nodes.length && services){nodes=services.map(function(s){return {service:s.name||s.service,status:s.status,error_rate:s.error_rate};});}
  var edgeColor  = cssVar('--chart-grid', '#94a3b8');
  var upColor    = cssVar('--green', '#22c55e');
  var downColor  = cssVar('--err', '#ef4444');
  var labelColor = cssVar('--text', '#1a1a2e');
  var panelColor = cssVar('--panel', cssVar('--bg', '#ffffff'));
  var accentColor= cssVar('--accent', upColor);
  var dpr=window.devicePixelRatio||1,w=c.clientWidth||800,h=340;
  c.width=w*dpr;c.height=h*dpr;
  var g=c.getContext('2d');g.setTransform(dpr,0,0,dpr,0,0);g.clearRect(0,0,w,h);
  if(!nodes.length){
    g.fillStyle=labelColor;g.globalAlpha=0.6;g.textAlign='center';g.font='13px system-ui, sans-serif';
    g.fillText('No services reporting yet',w/2,h/2);g.globalAlpha=1;
    var m0=$('#fleet-topology-meta');if(m0)m0.textContent='0 nodes · 0 edges';
    return;
  }
  var span=playback&&playback.index>=0?playback.spans[playback.index]:null;
  var activeService=span?span.service:'';
  // The entry point is where the user's request actually landed when a
  // trace is loaded, and otherwise the service nothing is observed calling.
  var entry='';
  if(playback&&playback.spans&&playback.spans.length) entry=playback.spans[0].service;
  if(!entry){
    var called={};
    edges.forEach(function(e){var to=e.callee||e.to;if(to)called[to]=1;});
    for(var i=0;i<nodes.length;i++){var nm=nodes[i].service||nodes[i].name;if(nm&&!called[nm]){entry=nm;break;}}
    if(!entry&&nodes.length) entry=nodes[0].service||nodes[0].name;
  }
  var pos=fleetTopoLayout(nodes,edges,entry,w,h);
  var phase=(Date.now()/900)%1;
  var hops=fleetHopTimings(playback);
  var cursor=playback?playback.index:-1;
  // Service-to-service edges first, so node cards paint over their ends.
  //
  // Every observed edge is drawn as a PAIR of arcs — the request going out
  // and the response coming back — on opposite sides of the line between
  // the two nodes (fleetCurve bows perpendicular to direction of travel,
  // so reversing the endpoints mirrors the arc for free). A single arrow
  // per pair, which is what this drew before, showed that A calls B but
  // never that B answered: on a failed hop there was nothing to colour
  // red, and mid-playback there was no way to see a call was still
  // outstanding versus already returned.
  edges.forEach(function(e){
    var from=e.caller||e.from, to=e.callee||e.to;
    var a=pos[from], b=pos[to]; if(!a||!b) return;
    var hop=hops[from+'\u0000'+to];
    // In flight while the cursor is inside the hop's spans; returned once
    // it has moved past the last of them.
    var inFlight=!!(hop&&cursor>=hop.first&&cursor<=hop.last);
    var returned=!!(hop&&cursor>hop.last);
    var failed=!!(hop&&hop.status>=500);

    var callCp=fleetCurve(a,b,18);
    fleetDrawEdge(g,a,b,callCp,inFlight?upColor:edgeColor,inFlight?3:1.6,inFlight?[]:[5,5]);
    if(inFlight) fleetDrawGlow(g,a,b,callCp,phase,upColor);

    var respCp=fleetCurve(b,a,18);
    var respColor=returned?(failed?downColor:accentColor):edgeColor;
    fleetDrawEdge(g,b,a,respCp,respColor,returned?2.4:1.2,returned?[]:[3,7]);
    if(returned&&cursor<=hop.last+1) fleetDrawGlow(g,b,a,respCp,phase,respColor);

    // Timing goes on the leg it describes: the measured hop when a trace
    // is open, otherwise the aggregate the topology endpoint already
    // computes over the retention window.
    var label='';
    if(hop) label=fmtMS(hop.ms)+(hop.calls>1?' \u00d7'+hop.calls:'');
    else if(e.p50_ms!=null||e.p95_ms!=null) label='p50 '+fmtMS(e.p50_ms||0)+' \u00b7 p95 '+fmtMS(e.p95_ms||0);
    else if(e.avg_ms!=null) label=fmtMS(e.avg_ms);

    fleetDrawEdgeLabel(g,a,b,callCp,label,panelColor,failed?downColor:labelColor);
  });

  // The user's own call, and the response back to them. Without these the
  // graph showed services talking to each other but never showed the
  // request entering or the answer leaving.
  var userPos=pos[FLEET_USER_KEY], entryPos=pos[entry];
  if(userPos&&entryPos){
    var callActive=!!(playback&&playback.index>=0&&!playback.finishedAt);
    var respActive=!!(playback&&playback.finishedAt);
    var callCp=fleetCurve(userPos,entryPos,-16);
    fleetDrawEdge(g,userPos,entryPos,callCp,callActive?upColor:edgeColor,callActive?3:1.6,callActive?[]:[5,5]);
    if(callActive&&activeService===entry) fleetDrawGlow(g,userPos,entryPos,callCp,phase,upColor);
    var respCp=fleetCurve(entryPos,userPos,-16);
    var respColor=respActive?((span&&span.status>=500)?downColor:accentColor):edgeColor;
    fleetDrawEdge(g,entryPos,userPos,respCp,respColor,respActive?3:1.2,respActive?[]:[3,7]);
    if(respActive) fleetDrawGlow(g,entryPos,userPos,respCp,phase,respColor);
  }
  nodes.forEach(function(n){
    var name=n.service||n.name, p=pos[name]; if(!p) return;
    var bad=(n.status==='down'||n.error_rate>.1);
    fleetDrawNode(g,name,fleetServiceKind(name),p,panelColor,bad?downColor:upColor,labelColor,name===activeService);
  });
  if(userPos) fleetDrawNode(g,'User','user',userPos,panelColor,accentColor,labelColor,false);
  var meta=$('#fleet-topology-meta');
  if(meta) meta.textContent=nodes.length+' nodes · '+edges.length+' edges'+(activeService?' · serving '+activeService:'');
  if(fleetTopoNeedsFrame(playback) && !_fleetRaf) _fleetRaf=requestAnimationFrame(fleetTopoFrame);
}

// ─── Overview ──────────────────────────────────────────────────────────
function initOverview(){
  if(S.snapshot) renderOverview();
  // Real-time updates are handled by WebSocket snapshots (pushed every 1s).
  // This interval is just a fallback to re-render if WS is disconnected.
  startPagePoll('overview', function(){ renderOverview(); return Promise.resolve(); }, 5000);
}
// The cards are built once and updated in place. They used to be rebuilt as HTML on every
// snapshot (once a second), which restarted any animation on them and discarded text selection.
var OV_CARDS = [
  {key:'requests_today', label:'Requests Today', get:function(m){return m.requests_today;}, fmt:function(v){return fmtNum(v);}},
  {key:'rps', label:'Requests / sec', get:function(m){return m.requests_per_sec;}, fmt:function(v){return fmtNum(v, 1);}},
  {key:'avg', label:'Avg Response', get:function(m){return m.avg_resp_time_ms;}, fmt:fmtMS},
  {key:'err', label:'Error Rate', get:function(m){return m.error_rate*100;}, fmt:function(v){return fmtNum(v, 2)+'%';}, state:function(m){return m.error_rate>0.05?'red':'';}},
  {key:'sessions', label:'Active Sessions', get:function(m){return m.active_sessions;}, fmt:function(v){return fmtNum(v);}},
  {key:'db', label:'DB Connections', get:function(m){return m.db_connections;}, fmt:function(v){return fmtNum(v);}},
  {key:'cache', label:'Cache Hit', get:function(m){return m.cache_hit_ratio*100;}, fmt:function(v){return fmtNum(v, 1)+'%';}},
  {key:'queue', label:'Queue Jobs', get:function(m){return m.queue_jobs;}, fmt:function(v){return fmtNum(v);}},
  {key:'goroutines', label:'Goroutines', get:function(m){return m.goroutines;}, fmt:function(v){return fmtNum(v);}},
  {key:'heap', label:'Heap Alloc', get:function(m){return m.heap_alloc;}, fmt:fmtBytes},
  {key:'sys', label:'Mem Sys', get:function(m){return m.sys;}, fmt:fmtBytes},
  {key:'cpu', label:'CPU Usage', get:function(m){return m.cpu_usage;}, fmt:function(v){return fmtNum(v, 1)+'%';}, hint:function(m){ return m.num_cpu ? 'of one core \u00b7 '+m.num_cpu+' cores' : 'live'; }},
];
function buildOverviewCards(container){
  container.textContent = '';
  OV_CARDS.forEach(function(c, i){
    container.appendChild(el('div', {class:'card reveal', 'data-ov':c.key, style:'--i:'+i}, [
      el('div', {class:'label', text:c.label}),
      el('div', {class:'value', text:'-'}),
      el('div', {class:'delta', text:'live'})
    ]));
  });
  container.setAttribute('data-built', '1');
}
function setCard(node, card, m){
  var valueEl = node.querySelector('.value');
  var raw = card.get(m);
  var shown = node._shown;
  var state = card.state ? card.state(m) : '';
  node.classList.toggle('red', state === 'red');
  var hint = node.querySelector('.delta');
  if(hint && card.hint){ var h = card.hint(m); if(hint.textContent !== h) hint.textContent = h; }
  if(typeof raw !== 'number' || !isFinite(raw)){
    node._raw = undefined; node._shown = undefined;
    if(valueEl.textContent !== '-') valueEl.textContent = '-';
    return;
  }
  if(node._raw === raw) return;
  node._raw = raw;
  if(node._stop) node._stop();
  var from = (typeof shown === 'number') ? shown : raw;
  node._stop = tween(from, raw, 520, function(v){
    node._shown = v;
    valueEl.textContent = card.fmt(v);
  });
  if(typeof shown === 'number' && motionOK()){
    // a short highlight says "this one just moved"
    node.classList.remove('tick');
    void node.offsetWidth;
    node.classList.add('tick');
  }
}
function renderOverview(){
  var m = S.snapshot ? S.snapshot.metrics : (S.history[S.history.length-1]||{});
  var container = $('#overview-cards');
  if(container){
    if(container.getAttribute('data-built') !== '1') buildOverviewCards(container);
    OV_CARDS.forEach(function(c){
      var node = container.querySelector('[data-ov="'+c.key+'"]');
      if(node) setCard(node, c, m);
    });
  }
  var times = S.history.map(function(h){return h.time;});
  drawLineChart($('#chart-rps'), S.history.map(function(h){return h.requests_per_sec||0;}), chartColor(1), {times:times, label:'Requests per second', fmt:function(v){return fmtNum(v, 1)+' req/s';}});
  drawLineChart($('#chart-latency'), S.history.map(function(h){return h.avg_resp_time_ms||0;}), chartColor(2), {times:times, label:'Response time', fmt:fmtMS});
  drawLineChart($('#chart-mem'), S.history.map(function(h){return (h.heap_alloc||0)/1024/1024;}), chartColor(3), {times:times, label:'Heap memory', fmt:function(v){return fmtNum(v, 1)+' MB';}});
  drawLineChart($('#chart-goroutines'), S.history.map(function(h){return h.goroutines||0;}), chartColor(4), {times:times, label:'Goroutines', fmt:function(v){return fmtNum(v);}});
}

// ─── Routes ────────────────────────────────────────────────────────────
function initRoutes(signal){
  api('routes', {signal:signal}).then(function(d){S.routes=d;renderRoutes();}).catch(function(){});
  var inp = $('#route-search');
  if(inp) pageOn(inp, 'input', function(){S._routeSearch=inp.value; renderRoutes(); inp.focus();});
}
function renderRoutes(){
  var routes = S.routes || [];
  var search = (S._routeSearch||'').toLowerCase();
  var filtered = routes.filter(function(r){
    if(!search) return true;
    return (r.pattern||'').toLowerCase().indexOf(search)>=0 || (r.method||'').toLowerCase().indexOf(search)>=0;
  });
  var c = $('#route-count');
  if(c) c.textContent = filtered.length;
  var html = '';
  filtered.forEach(function(r){
    html += '<tr><td><span class="method-pill '+cls(r.method)+'">'+escapeHTML(r.method)+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(r.pattern)+'</td>'+
      '<td>'+fmtNum(r.requests)+'</td><td>'+fmtMS(r.avg_latency_ms)+'</td><td>'+fmtMS(r.max_latency_ms)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(r.last_request?fmtDate(r.last_request):'-')+'</td>'+
      '<td>'+(r.errors>0?'<span class="badge red">'+num(r.errors)+'</span>':'-')+'</td></tr>';
  });
  if(!filtered.length) html = '<tr><td colspan="7" class="empty">No routes registered</td></tr>';
  var tb = $('#routes-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── API Explorer ──────────────────────────────────────────────────────
function initAPI(signal){
  if(!S.apiRoutes.length){
    api('api-explorer', {signal:signal}).then(function(d){S.apiRoutes=d||[];renderAPIExplorer();}).catch(function(){});
  } else {
    renderAPIExplorer();
  }
}
// What the person has typed is kept per endpoint. The form used to be rebuilt from the route on
// every render, so sending a request cleared its body, headers and edited URL, and the custom
// headers could not be sent at all (the selector that gathered them matched nothing).
function apiFormFor(r){
  var key = r.method + ' ' + r.path;
  var f = S.apiForm[key];
  if(f) return f;
  f = {method:r.method, url:r.path, headers:[{k:'',v:''}], body:'', params:{}, query:{}};
  // A pattern such as /users/{id} or /users/:id needs a value for id before it can be sent. A route
  // with documented inputs lists them below; an undocumented one still has them in its path.
  (String(r.path).match(/\{[A-Za-z0-9_]+\}|:[A-Za-z0-9_]+/g) || []).forEach(function(tok){
    var name = tok.replace(/[{}:]/g, '');
    if(!(name in f.params)) f.params[name] = '';
  });
  (r.inputs||[]).forEach(function(g){
    var fields = g.fields || {};
    Object.keys(fields).sort().forEach(function(name){
      var fs = fields[name] || {};
      var seed = fs.default != null ? String(fs.default) : '';
      if(g.type === 'params') f.params[name] = seed;
      else if(g.type === 'query') f.query[name] = seed;
      else if(g.type === 'header') f.headers.unshift({k:name, v:seed});
    });
    if(g.type === 'body' && Object.keys(fields).length){
      var sample = {};
      Object.keys(fields).sort().forEach(function(name){
        var fs = fields[name] || {};
        if(fs.default != null) sample[name] = fs.default;
        else sample[name] = ({integer:0, number:0, boolean:false, array:[], object:{}})[fs.type] != null ? ({integer:0, number:0, boolean:false, array:[], object:{}})[fs.type] : '';
      });
      f.body = JSON.stringify(sample, null, 2);
      if(!f.headers.some(function(h){return h.k.toLowerCase()==='content-type';})) f.headers.push({k:'Content-Type', v:'application/json'});
    }
  });
  S.apiForm[key] = f;
  return f;
}
// apiTarget builds the URL that will be sent: path parameters substituted, query parameters appended.
function apiTarget(r, f){
  var url = f.url;
  Object.keys(f.params).forEach(function(name){
    if(f.params[name] !== '') url = url.split('{'+name+'}').join(encodeURIComponent(f.params[name])).split(':'+name).join(encodeURIComponent(f.params[name]));
  });
  var q = Object.keys(f.query).filter(function(k){return f.query[k] !== '';}).map(function(k){
    return encodeURIComponent(k)+'='+encodeURIComponent(f.query[k]);
  }).join('&');
  if(q) url += (url.indexOf('?')>=0 ? '&' : '?') + q;
  return url;
}
function copyText(text){
  if(navigator.clipboard && window.isSecureContext){
    return navigator.clipboard.writeText(text);
  }
  // http:// on a LAN address has no clipboard API
  return new Promise(function(resolve, reject){
    var ta = el('textarea', {style:'position:fixed;opacity:0;top:0', 'aria-hidden':'true'});
    ta.value = text; document.body.appendChild(ta); ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch(e){}
    document.body.removeChild(ta);
    ok ? resolve() : reject(new Error('copy is not available here'));
  });
}
function renderAPIExplorer(){
  var sel = S.apiRouteSel;
  var c = $('#api-detail');
  if(!c) return;
  var filter = (S.apiFilter||'').toLowerCase();
  var listHtml = '<div class="api-list-head"><span>Endpoints (<span id="api-count">'+S.apiRoutes.length+'</span>)</span>'+
    '<input id="api-filter" type="search" placeholder="Filter" aria-label="Filter endpoints" value="'+attr(S.apiFilter)+'"></div>';
  S.apiRoutes.forEach(function(r, i){
    if(filter && (r.path+' '+r.method+' '+(r.summary||'')).toLowerCase().indexOf(filter) < 0) return;
    listHtml += '<div class="item '+(sel===i?'active':'')+'" data-idx="'+i+'" role="button" tabindex="0">'+
      '<span class="method-pill '+cls(r.method)+'">'+escapeHTML(r.method)+'</span>'+
      '<span style="font-family:var(--mono);font-size:11px">'+escapeHTML(r.path)+'</span></div>';
  });
  var listEl = $('#api-list');
  // Re-rendering the list rebuilt the filter box too, which lost focus on every keystroke. It is
  // only rebuilt when the endpoint selection or the response changes; typing patches the items.
  if(listEl){
    var keepFocus = document.activeElement && document.activeElement.id === 'api-filter';
    listEl.innerHTML = listHtml;
    if(keepFocus){
      var fi = $('#api-filter'); fi.focus();
      try { fi.setSelectionRange(fi.value.length, fi.value.length); } catch(e){}
    }
  }
  var filterEl = $('#api-filter');
  if(filterEl) pageOn(filterEl, 'input', function(){ S.apiFilter = filterEl.value; renderAPIExplorer(); });
  $$('#api-list .item').forEach(function(it){
    var go = function(){
      S.apiRouteSel = parseInt(it.dataset.idx, 10);
      S.apiResp = null;
      renderAPIExplorer();
    };
    pageOn(it, 'click', go);
    pageOn(it, 'keydown', function(e){ if(e.key==='Enter' || e.key===' '){ e.preventDefault(); go(); } });
  });

  var html = '';
  var r = sel!=null ? S.apiRoutes[sel] : null;
  var f = r ? apiFormFor(r) : null;
  if(r){
    html += '<div class="api-form">';
    if(r.summary || r.description){
      html += '<div class="api-doc"><strong>'+escapeHTML(r.summary||'')+'</strong>'+(r.description?'<p>'+escapeHTML(r.description)+'</p>':'')+'</div>';
    }
    html += '<div class="row"><label for="api-method">Method</label><div class="controls"><select id="api-method">'+
      ['GET','POST','PUT','PATCH','DELETE','HEAD','OPTIONS'].map(function(m){return '<option '+(m===f.method?'selected':'')+'>'+m+'</option>';}).join('')+
      '</select></div></div>'+
      '<div class="row"><label for="api-url">URL</label><div class="controls"><input id="api-url" value="'+attr(f.url)+'" placeholder="/path" spellcheck="false"></div></div>';
    Object.keys(f.params).forEach(function(name){
      html += '<div class="row"><label for="p-'+attr(name)+'">'+escapeHTML(name)+' <small>path</small></label><div class="controls"><input data-param="'+attr(name)+'" id="p-'+attr(name)+'" value="'+attr(f.params[name])+'" placeholder="value for {'+attr(name)+'}" spellcheck="false"></div></div>';
    });
    Object.keys(f.query).forEach(function(name){
      html += '<div class="row"><label for="q-'+attr(name)+'">'+escapeHTML(name)+' <small>query</small></label><div class="controls"><input data-query="'+attr(name)+'" id="q-'+attr(name)+'" value="'+attr(f.query[name])+'" spellcheck="false"></div></div>';
    });
    html += '<div class="row"><label>Headers</label><div><div class="headers" id="api-headers">';
    f.headers.forEach(function(h, i){
      html += '<input class="hk" data-i="'+i+'" placeholder="Key" aria-label="Header name" value="'+attr(h.k)+'" spellcheck="false"><input class="hv" data-i="'+i+'" placeholder="Value" aria-label="Header value" value="'+attr(h.v)+'" spellcheck="false"><button class="rm" data-i="'+i+'" aria-label="Remove header" type="button">-</button>';
    });
    html += '</div><button id="api-add-header" type="button" style="margin-top:6px">+ Header</button></div></div>'+
      '<div class="row"><label for="api-body">Body</label><div><textarea id="api-body" rows="6" placeholder="{}" spellcheck="false">'+escapeHTML(f.body)+'</textarea></div></div>'+
      '<div class="row"><label></label><div><button class="primary" id="api-send" type="button">Send</button> <span class="api-hint">Ctrl/\u2318 + Enter</span></div></div>'+
      '</div>';
    if(S.apiResp){
      var r2 = S.apiResp;
      html += '<div class="api-response reveal-in"><div class="head"><div><span class="status-pill '+statusClass(r2.status)+'">'+num(r2.status)+'</span> '+
        '<span style="margin-left:8px;font-size:11px;color:var(--text-dim)">'+fmtMS(r2.duration_ms)+' \u00b7 '+fmtBytes(r2.size)+'</span></div>'+
        '<button id="api-clear" type="button">Clear</button></div>'+
        '<div class="body"><pre>'+escapeHTML(r2.body_json?JSON.stringify(r2.body_json, null, 2):r2.body)+'</pre></div></div>';
      html += '<div class="snippets reveal-in"><div class="tabs" role="tablist">'+
        ['curl','go','javascript','python','csharp','php'].map(function(l){
          return '<div class="tab '+(S.apiSnippetLang===l?'active':'')+'" data-lang="'+l+'" role="tab" tabindex="0" aria-selected="'+(S.apiSnippetLang===l)+'">'+l+'</div>';
        }).join('')+
        '</div><div class="tab-content"><button class="copy-btn" type="button">Copy</button><pre>'+
        escapeHTML((r2.snippets||{})[S.apiSnippetLang]||'')+
        '</pre></div></div>';
    }
  } else {
    html = '<div class="empty"><div class="icon">\u2197</div>Select an endpoint to begin</div>';
  }
  c.innerHTML = html;
  if(!r) return;

  // Keep the form object in step with what is typed.
  var bind = function(sel2, fn){ $$(sel2).forEach(function(n){ pageOn(n, 'input', function(){ fn(n); }); }); };
  var m = $('#api-method'); if(m) pageOn(m, 'change', function(){ f.method = m.value; });
  var u = $('#api-url'); if(u) pageOn(u, 'input', function(){ f.url = u.value; });
  bind('[data-param]', function(n){ f.params[n.dataset.param] = n.value; });
  bind('[data-query]', function(n){ f.query[n.dataset.query] = n.value; });
  $$('#api-headers .hk').forEach(function(n){ pageOn(n, 'input', function(){ f.headers[n.dataset.i].k = n.value; }); });
  $$('#api-headers .hv').forEach(function(n){ pageOn(n, 'input', function(){ f.headers[n.dataset.i].v = n.value; }); });
  var bodyEl = $('#api-body'); if(bodyEl) pageOn(bodyEl, 'input', function(){ f.body = bodyEl.value; });

  var addHdr = $('#api-add-header');
  if(addHdr) pageOn(addHdr, 'click', function(){
    f.headers.push({k:'', v:''});
    renderAPIExplorer();
    var ks = $$('#api-headers .hk'); if(ks.length) ks[ks.length-1].focus();
  });
  $$('#api-headers .rm').forEach(function(b){
    pageOn(b, 'click', function(){
      f.headers.splice(parseInt(b.dataset.i, 10), 1);
      if(!f.headers.length) f.headers.push({k:'', v:''});
      renderAPIExplorer();
    });
  });
  var sendBtn = $('#api-send');
  var send = function(){
    var headers = {};
    f.headers.forEach(function(h){ if(h.k.trim()) headers[h.k.trim()] = h.v; });
    var unfilled = Object.keys(f.params).filter(function(n){ return f.params[n] === '' && (f.url.indexOf('{'+n+'}') >= 0 || f.url.indexOf(':'+n) >= 0); });
    if(unfilled.length){
      toast('Fill in the path parameter' + (unfilled.length > 1 ? 's' : '') + ': ' + unfilled.join(', '), 'error');
      var firstEmpty = $('[data-param="' + unfilled[0].replace(/"/g, '\\"') + '"]');
      if(firstEmpty) firstEmpty.focus();
      return;
    }
    var payload = { method: f.method, url: apiTarget(r, f), headers: headers, body: f.body };
    sendBtn.textContent = 'Sending...';
    sendBtn.disabled = true;
    apiPost('api-explorer', payload, {signal:currentPageSignal()}).then(function(resp){
      S.apiResp = resp;
      renderAPIExplorer();
      var out = $('.api-response'); if(out && out.scrollIntoView) out.scrollIntoView({block:'nearest', behavior: motionOK() ? 'smooth' : 'auto'});
    }).catch(function(e){
      if(e && e.name === 'AbortError') return;
      sendBtn.textContent = 'Send';
      sendBtn.disabled = false;
      toast('Request failed: ' + (e && e.message ? e.message : 'unknown error'), 'error');
    });
  };
  if(sendBtn) pageOn(sendBtn, 'click', send);
  var form = $('.api-form');
  if(form) pageOn(form, 'keydown', function(e){ if((e.ctrlKey||e.metaKey) && e.key==='Enter'){ e.preventDefault(); send(); } });
  var clearBtn = $('#api-clear');
  if(clearBtn) pageOn(clearBtn, 'click', function(){S.apiResp=null; renderAPIExplorer();});
  $$('.snippets .tab').forEach(function(t){
    var pick = function(){ S.apiSnippetLang = t.dataset.lang; renderAPIExplorer(); };
    pageOn(t, 'click', pick);
    pageOn(t, 'keydown', function(e){ if(e.key==='Enter' || e.key===' '){ e.preventDefault(); pick(); } });
  });
  var copyBtn = $('.copy-btn');
  if(copyBtn) pageOn(copyBtn, 'click', function(){
    copyText($('.snippets pre').textContent).then(function(){
      copyBtn.textContent='Copied!'; setTimeout(function(){copyBtn.textContent='Copy';},1500);
    }).catch(function(){ toast('Copy is not available in this browser context', 'error'); });
  });
}

// ─── Stat cards ────────────────────────────────────────────────────────
// renderCards keeps a row of stat cards in place. The nodes are created once (with a staggered
// entrance) and a later render touches only the text that changed, so a card that is refreshed
// every few seconds neither flickers nor replays its entrance.
function renderCards(container, cards){
  if(!container) return;
  var have = container._cards;
  if(!have || have.length !== cards.length || have.some(function(e, i){ return e.key !== cards[i].label; })){
    container.textContent = '';
    have = cards.map(function(c, i){
      var valueEl = el('div', {class:'value'});
      var node = el('div', {class:'card reveal', style:'--i:'+i}, [el('div', {class:'label', text:c.label}), valueEl]);
      container.appendChild(node);
      return {key:c.label, node:node, valueEl:valueEl, text:null};
    });
    container._cards = have;
  }
  cards.forEach(function(c, i){
    var e = have[i];
    var text = String(c.value);
    if(e.text !== text){
      if(e.text !== null && motionOK()){ e.node.classList.remove('tick'); void e.node.offsetWidth; e.node.classList.add('tick'); }
      e.valueEl.textContent = text;
      e.text = text;
    }
    e.valueEl.style.color = c.color || '';
    e.valueEl.style.fontSize = c.small ? '18px' : '';
    e.node.classList.toggle('red', c.cls === 'red');
    e.node.classList.toggle('yellow', c.cls === 'yellow');
  });
}

// ─── Live Requests ─────────────────────────────────────────────────────
function initRequests(signal){
  loadRequests(signal);
  renderRequests();
  ['f-method','f-status','f-route','f-user'].forEach(function(id){
    var e = $('#'+id);
    if(!e) return;
    var evt = id==='f-method'||id==='f-status' ? 'change' : 'input';
    pageOn(e, evt, function(){
      if(id==='f-method') S.reqFilter.method=e.value;
      if(id==='f-status') S.reqFilter.status=e.value;
      if(id==='f-route') S.reqFilter.route=e.value;
      if(id==='f-user') S.reqFilter.user=e.value;
      renderRequests();
      if(evt==='input') e.focus();
    });
  });
  var pb = $('#req-pause');
  if(pb) pageOn(pb, 'click', toggleReqPause);
  var cb = $('#req-clear');
  if(cb) pageOn(cb, 'click', function(){ S.requests = []; S.reqBuffer = []; renderRequests(); updateReqPaused(); });
  updateReqPaused();
  startPagePoll('requests', loadRequests, 10000);
}
// Pause freezes the table so a row can be read or clicked while traffic keeps arriving; what
// arrives meanwhile is held, counted on the button, and folded in on resume.
function toggleReqPause(){
  S.reqPaused = !S.reqPaused;
  if(!S.reqPaused){
    S.reqBuffer.forEach(function(r){ S.requests.push(r); if(r && r.id) S._fresh[r.id] = true; });
    S.reqBuffer = [];
    while(S.requests.length > 500) S.requests.shift();
    renderRequests();
  }
  updateReqPaused();
}
function updateReqPaused(){
  var b = $('#req-pause');
  if(!b) return;
  b.textContent = S.reqPaused ? ('Resume' + (S.reqBuffer.length ? ' (' + S.reqBuffer.length + ' new)' : '')) : 'Pause';
  b.setAttribute('aria-pressed', S.reqPaused ? 'true' : 'false');
  b.classList.toggle('active', S.reqPaused);
}
function loadRequests(signal){
  // A frozen table is not replaced by a poll.
  if(S.reqPaused) return Promise.resolve();
  return api('requests?limit=200', {signal:signal}).then(function(d){S.requests=d||[]; renderRequests();}).catch(function(){});
}
function renderRequests(){
  var list = S.requests.slice().reverse();
  var f = S.reqFilter;
  if(f.method) list = list.filter(function(r){return r.method===f.method;});
  if(f.status) list = list.filter(function(r){return (''+r.status).startsWith(f.status.toLowerCase().replace('xx',''));});
  if(f.route) list = list.filter(function(r){return (r.route||r.path||'').indexOf(f.route)>=0;});
  if(f.user) list = list.filter(function(r){return (r.user||'').toLowerCase().indexOf(f.user.toLowerCase())>=0;});
  var c = $('#req-count');
  if(c) c.textContent = list.length;
  var html = '';
  var max = 200;
  // "Slow" is the configured threshold, which the server sends on each record and in its
  // capabilities; the page used to hard-code 500 ms whatever slow_request_ms said.
  var slowMS = S.caps.slow_request_ms;
  list.slice(0, max).forEach(function(r){
    var durSlow = r.slow || (slowMS > 0 && r.duration_ms >= slowMS);
    var fresh = r.id && S._fresh[r.id];
    html += '<tr'+(fresh?' class="row-new"':'')+'><td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(r.time)+'</td>'+
      '<td><span class="method-pill '+cls(r.method)+'">'+escapeHTML(r.method)+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:340px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(r.path)+'</td>'+
      '<td><span class="status-pill '+statusClass(r.status)+'">'+(r.status?num(r.status):'-')+'</span></td>'+
      '<td>'+(durSlow?'<span style="color:var(--err)">'+fmtMS(r.duration_ms)+'</span>':fmtMS(r.duration_ms))+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+escapeHTML(r.ip)+'</td>'+
      '<td style="font-size:11px">'+escapeHTML(r.user||'-')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+fmtBytes(r.resp_size)+'</td>'+
      // The link only when a timeline was actually recorded: a request nobody was watching has an
      // id but no timeline, and "view" led to an empty page.
      '<td>'+(r.has_timeline && r.timeline_id?'<a href="'+attr(S.base)+'/timeline" data-tl="'+attr(r.timeline_id)+'">view</a>':'-')+'</td></tr>';
  });
  if(list.length>max) html += '<tr><td colspan="9" style="text-align:center;color:var(--text-muted);padding:8px">Showing latest '+max+' of '+list.length+'</td></tr>';
  if(!list.length) html = '<tr><td colspan="9" class="empty">'+(S.requests.length ? 'No requests match these filters' : 'No requests yet — send one to this service and it appears here')+'</td></tr>';
  var tb = $('#req-tbody');
  if(tb) tb.innerHTML = html;
  S._fresh = {};
  $$('a[data-tl]').forEach(function(a){
    pageOn(a, 'click', function(e){
      e.preventDefault();
      S._tlSelected = a.dataset.tl;
      if(window.breeze) breeze.navigate(S.base+'/timeline');
      else location.href = S.base+'/timeline';
    });
  });
}

// ─── ORM Queries ───────────────────────────────────────────────────────
function initQueries(signal){
  api('queries?limit=200', {signal:signal}).then(function(d){S.queries=d||[]; renderQueries();}).catch(function(){});
  renderQueries();
  var so = $('#slow-only');
  if(so) pageOn(so, 'change', function(){S._slowOnly=so.checked; renderQueries();});
  var si = $('#q-search');
  if(si) pageOn(si, 'input', function(){S.qSearch=si.value; renderQueries(); si.focus();});
  startPagePoll('queries', function(signal){
    return api('queries?limit=200', {signal:signal}).then(function(d){S.queries=d||[]; renderQueries();}).catch(function(){});
  }, 5000);
}
function renderQueries(){
  var list = S.queries.slice().reverse();
  if(S._slowOnly) list = list.filter(function(q){return q.slow;});
  var q = S.qSearch.toLowerCase();
  if(q) list = list.filter(function(x){return (x.sql||'').toLowerCase().indexOf(q)>=0;});
  var c = $('#q-count');
  if(c) c.textContent = list.length;
  var html = '';
  list.slice(0,300).forEach(function(q){
    html += '<tr><td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(q.time)+'</td>'+
      '<td class="query-sql">'+escapeHTML(q.sql)+'</td>'+
      '<td>'+(q.slow?'<span class="badge red">slow</span> ':'')+fmtDur(q.duration_us)+'</td>'+
      '<td style="font-family:var(--mono)">'+fmtNum(q.rows)+'</td>'+
      '<td style="font-family:var(--mono);font-size:10px;color:var(--text-dim)">'+escapeHTML(q.file)+(q.file?':'+num(q.line):'')+'</td>'+
      '<td>'+(q.error?'<span class="badge red">error</span>':'<span class="badge green">ok</span>')+'</td></tr>';
  });
  if(!list.length) html = '<tr><td colspan="6" class="empty">'+(S.queries.length ? 'No queries match' : 'No queries captured yet')+'</td></tr>';
  var tb = $('#q-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── Cache ─────────────────────────────────────────────────────────────
function initCache(signal){
  loadCache(signal);
  var cc = $('#cache-clear');
  if(cc) pageOn(cc, 'click', function(){ clearCache('', cc); });
  var cp = $('#cache-clear-prefix');
  if(cp) pageOn(cp, 'click', function(){
    var p = ($('#cache-prefix').value || '').trim();
    if(!p){ toast('Enter a key prefix first', 'error'); return; }
    clearCache(p, cp);
  });
  startPagePoll('cache', loadCache, 5000);
}
function clearCache(prefix, btn){
  if(!prefix && !window.confirm('Clear the entire cache?')) return;
  btn.disabled = true;
  apiPost('cache/clear', {prefix:prefix}, {signal:currentPageSignal()}).then(function(d){
    toast(prefix ? 'Cleared ' + num(d && d.cleared) + ' entries starting with "' + prefix + '"' : 'Cache cleared (' + num(d && d.cleared) + ' entries)', 'ok');
    loadCache(currentPageSignal());
  }).catch(function(e){
    if(e && e.name === 'AbortError') return;
    // 501 means the app never connected a cache; the buttons used to say "ok" while doing nothing.
    toast(e && e.status === 501 ? 'No cache is attached to the dashboard yet. Connect one with Collector.SetCacheClearer.' : 'Could not clear the cache: ' + (e && e.message), 'error', 7000);
  }).then(function(){ btn.disabled = false; });
}
function loadCache(signal){
  return api('cache', {signal:signal}).then(function(d){S.cache=d; renderCache();}).catch(function(){});
}
function renderCache(){
  var c = S.cache || {};
  renderCards($('#cache-cards'), [
    {label:'Driver', value:c.driver||'-', small:true},
    // The counter is every lookup, hits plus misses: it was labelled "Keys", which it never was.
    {label:'Lookups', value:fmtNum(c.keys)},
    {label:'Hits', value:fmtNum(c.hits), color:'var(--green)'},
    {label:'Misses', value:fmtNum(c.misses), color:'var(--err)'},
    {label:'Hit Rate', value:fmtNum(c.hit_rate*100, 1)+'%', color:'var(--primary)'},
    {label:'Memory', value:fmtBytes(c.memory_bytes)}
  ]);
  var tip = S.caps.can_clear_cache ? '' : 'No cache is attached: connect one with Collector.SetCacheClearer';
  ['cache-clear', 'cache-clear-prefix'].forEach(function(id){ var b = $('#'+id); if(b) b.title = tip; });
  var times = S.history.map(function(h){return h.time;});
  drawLineChart($('#chart-cache'), S.history.map(function(h){return (h.cache_hit_ratio||0)*100;}), chartColor(2), {times:times, label:'Cache hit ratio', fmt:function(v){return fmtNum(v, 1)+'%';}});
}

// ─── Queue ─────────────────────────────────────────────────────────────
function initQueue(signal){
  loadQueue(signal);
  startPagePoll('queue', loadQueue, 5000);
}
function loadQueue(signal){
  return api('queue', {signal:signal}).then(function(d){S.queue=d; renderQueue();}).catch(function(){});
}
function renderQueue(){
  var q = S.queue || {};
  var s = q.summary || {};
  var jobs = q.jobs || [];
  renderCards($('#queue-cards'), [
    {label:'Pending', value:fmtNum(s.pending), color:'var(--yellow)'},
    {label:'Running', value:fmtNum(s.running), color:'var(--primary)'},
    {label:'Completed', value:fmtNum(s.completed), color:'var(--green)'},
    {label:'Failed', value:fmtNum(s.failed), color:'var(--err)'}
  ]);
  var jc = $('#job-count');
  if(jc) jc.textContent = jobs.length;
  var html = '';
  jobs.forEach(function(j){
    var badge = {pending:'yellow', running:'blue', completed:'green', failed:'red'}[j.state]||'gray';
    html += '<tr><td style="font-family:var(--mono);font-size:11px">'+escapeHTML(j.id)+'</td>'+
      '<td>'+escapeHTML(j.queue)+'</td><td><span class="badge '+badge+'">'+escapeHTML(j.state)+'</span></td>'+
      '<td>'+num(j.attempts)+'</td><td style="font-size:11px;color:var(--text-dim)">'+fmtDate(j.queued_at)+'</td>'+
      '<td>'+(j.duration_ms?fmtMS(j.duration_ms):'-')+'</td>'+
      '<td style="font-size:11px;color:var(--err)">'+escapeHTML(j.error||'')+'</td>'+
      // The id goes into an attribute, and it is whatever the application called the job.
      '<td>'+(j.state==='failed'&&q.can_retry?'<button class="retry" data-id="'+attr(j.id)+'">Retry</button>':(j.state==='failed'?'<span title="Connect your queue with Collector.SetJobRetrier to retry from here" style="color:var(--text-muted)">n/a</span>':'-'))+'</td></tr>';
  });
  if(!jobs.length) html = '<tr><td colspan="8" class="empty">No jobs registered. Report them with Collector.RegisterJob / UpdateJob.</td></tr>';
  var tb = $('#job-tbody');
  if(tb) tb.innerHTML = html;
  $$('.retry').forEach(function(b){
    pageOn(b, 'click', function(){
      b.disabled = true;
      apiPost('queue/retry', {id:b.dataset.id}, {signal:currentPageSignal()}).then(function(){
        toast('Job ' + b.dataset.id + ' queued for retry', 'ok');
        loadQueue(currentPageSignal());
      }).catch(function(e){
        if(e && e.name === 'AbortError') return;
        b.disabled = false;
        toast(e && e.status === 501 ? 'No queue is attached to the dashboard (Collector.SetJobRetrier).' : 'Retry failed: ' + (e && e.message), 'error', 6000);
      });
    });
  });
}

// ─── Scheduler ─────────────────────────────────────────────────────────
function initScheduler(signal){
  api('scheduler', {signal:signal}).then(function(d){S.tasks=d||[]; renderScheduler();}).catch(function(){});
  startPagePoll('scheduler', function(signal){
    return api('scheduler', {signal:signal}).then(function(d){S.tasks=d||[]; renderScheduler();}).catch(function(){});
  }, 5000);
}
function renderScheduler(){
  var tasks = S.tasks || [];
  var c = $('#task-count');
  if(c) c.textContent = tasks.length;
  var html = '';
  tasks.forEach(function(t){
    var badge = {idle:'gray', running:'blue', failed:'red', ok:'green'}[t.status]||'gray';
    html += '<tr><td><strong>'+escapeHTML(t.name)+'</strong></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(t.cron)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(t.last_run?fmtDate(t.last_run):'-')+'</td>'+
      '<td>'+(t.last_run_ms?fmtMS(t.last_run_ms):'-')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(t.next_run?fmtDate(t.next_run):'-')+'</td>'+
      '<td><span class="badge '+badge+'">'+escapeHTML(t.status||'-')+'</span></td>'+
      '<td>'+fmtNum(t.run_count)+'</td>'+
      '<td>'+(t.fail_count>0?'<span class="badge red">'+num(t.fail_count)+'</span>':'-')+'</td></tr>';
  });
  if(!tasks.length) html = '<tr><td colspan="8" class="empty">No tasks registered. Report them with Collector.RegisterTask / UpdateTask.</td></tr>';
  var tb = $('#task-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── Logs ──────────────────────────────────────────────────────────────
function loadLogs(signal){
  var tab = S.logTab;
  // The counts of the tabs that are not open come from one cheap call; they read 0 until the tab was opened.
  api('logs/counts', {signal:signal}).then(function(d){ S.logCounts = d || null; renderLogCounts(); }).catch(function(){});
  return api('logs?level='+tab+'&limit=500', {signal:signal}).then(function(d){S.logs[tab]=d||[]; renderLogs();}).catch(function(){});
}
function initLogs(signal){
  $$('.log-tab').forEach(function(t){
    pageOn(t, 'keydown', function(e){ if(e.key==='Enter' || e.key===' '){ e.preventDefault(); t.click(); } });
  });
  $$('.log-tab').forEach(function(t){pageOn(t, 'click', function(){
    S.logTab=t.dataset.tab;
    $$('.log-tab').forEach(function(x){x.classList.remove('active'); x.setAttribute('aria-selected', 'false');});
    t.classList.add('active');
    t.setAttribute('aria-selected', 'true');
    // Load the newly-active tab's data.
    loadLogs(currentPageSignal());
    renderLogs();
  });});
  var s = $('#log-search');
  if(s) pageOn(s, 'input', function(){S.logSearch=s.value; renderLogs(); s.focus();});
  // Poll only the active tab every 10 seconds (not all 5 tabs).
  startPagePoll('logs', loadLogs, 10000);
  // Initial load — only the active tab.
  loadLogs(signal);
}
function renderLogCounts(){
  ['app','http','error','panic','warning'].forEach(function(l){
    var c = $('#log-count-'+l);
    if(!c) return;
    var n = (S.logCounts && S.logCounts[l] != null) ? S.logCounts[l] : (S.logs[l]||[]).length;
    c.textContent = n;
  });
}
function renderLogs(){
  var tab = S.logTab;
  var list = (S.logs[tab]||[]).slice().reverse();
  var q = S.logSearch.toLowerCase();
  if(q) list = list.filter(function(e){return (e.message||'').toLowerCase().indexOf(q)>=0;});
  renderLogCounts();
  var html = '';
  list.slice(0,500).forEach(function(e){
    html += '<div class="log-line"><span class="time">'+fmtTime(e.time)+'</span>'+
      '<span class="level '+cls(tab)+'">'+escapeHTML(e.level||tab)+'</span>'+
      '<span class="msg">'+escapeHTML(e.message)+(e.source?'<span style="color:var(--text-muted);margin-left:8px">'+escapeHTML(e.source)+'</span>':'')+'</span></div>';
  });
  if(!list.length) html = '<div class="empty">'+((S.logs[tab]||[]).length ? 'No log lines match' : 'No log entries')+'</div>';
  var v = $('#log-viewer');
  if(v) v.innerHTML = html;
}

// ─── Health ────────────────────────────────────────────────────────────
function initHealth(signal){
  loadHealth(signal);
  var rb = $('#health-refresh');
  if(rb) pageOn(rb, 'click', function(){
    rb.disabled = true;
    rb.setAttribute('aria-busy', 'true');
    loadHealth(currentPageSignal()).then(function(){ rb.disabled = false; rb.removeAttribute('aria-busy'); });
  });
  // Each refresh runs every probe (a database ping, a cache round trip). Ten seconds, not five.
  startPagePoll('health', loadHealth, 10000);
}
function loadHealth(signal){
  return api('health', {signal:signal}).then(function(d){S.health=d||[]; renderHealth();}).catch(function(){});
}
function healthIcon(state){ return state==='green'?'\u2713':(state==='yellow'?'\u26a0':'\u2717'); }
function healthMeta(h){ return fmtDur(h.latency_us)+' \u00b7 '+fmtTime(h.checked); }
function renderHealth(){
  // The status becomes a CSS class, and it is whatever the probe returned. The server maps it onto
  // green/yellow/red; the page does the same so a probe that says anything else cannot inject markup.
  var list = (S.health || []).map(function(h){ return Object.assign({}, h, {state: healthState(h.status)}); });
  var green=list.filter(function(h){return h.state==='green';}).length;
  var yellow=list.filter(function(h){return h.state==='yellow';}).length;
  var red=list.filter(function(h){return h.state==='red';}).length;
  renderCards($('#health-cards'), [
    {label:'Healthy', value:green, color:'var(--green)'},
    {label:'Warnings', value:yellow, color:'var(--yellow)'},
    {label:'Failing', value:red, color:'var(--err)'}
  ]);
  var g = $('#health-grid');
  if(!g) return;
  if(!list.length){
    g.innerHTML = '<div class="empty" style="grid-column:1/-1"><div class="icon">\u2764</div>No health checks registered. Add one with Collector.RegisterHealthCheck.</div>';
    return;
  }
  if(g.querySelector('.empty')) g.textContent = '';
  reconcileList(g, list, function(h){return h.name;},
    function(h){return h.state+'|'+h.message+'|'+h.latency_us+'|'+h.checked;},
    function(h){
      var node = el('div', {class:'health-card '+h.state}, [
        el('div', {class:'indicator', text:healthIcon(h.state)}),
        el('div', {class:'info'}, [
          el('div', {class:'name', text:h.name}),
          el('div', {class:'msg', 'data-role':'message', text:h.message||''}),
          el('div', {class:'msg', 'data-role':'meta', style:'margin-top:4px;font-size:10px', text:healthMeta(h)})
        ])
      ]);
      node.setAttribute('data-state', h.state);
      return node;
    },
    function(node, h){
      var changed = node.getAttribute('data-state') !== h.state;
      node.className = 'health-card '+h.state;
      node.setAttribute('data-state', h.state);
      node.querySelector('.indicator').textContent = healthIcon(h.state);
      node.querySelector('[data-role="message"]').textContent = h.message||'';
      node.querySelector('[data-role="meta"]').textContent = healthMeta(h);
      if(changed && motionOK()){ node.classList.add('changed'); setTimeout(function(){ node.classList.remove('changed'); }, 1100); }
    });
}

// ─── Performance ───────────────────────────────────────────────────────
function initPerformance(signal){
  loadPerf(signal);
  startPagePoll('performance', loadPerf, 5000);
}
function loadPerf(signal){
  return api('performance', {signal:signal}).then(function(d){S.perf=d; renderPerformance();}).catch(function(){});
}
function renderPerformance(){
  var p = S.perf || {};
  var cur = p.current || {};
  var hist = p.history || [];
  var rt = cur.runtime_tuning || {};
  var cards = [
    {label:'Goroutines', value:fmtNum(cur.goroutines||0)},
    {label:'Heap Alloc', value:fmtBytes(cur.heap?cur.heap.alloc:0)},
    {label:'Heap Idle', value:fmtBytes(cur.heap?cur.heap.idle:0)},
    {label:'Heap Released', value:fmtBytes(cur.heap?cur.heap.released:0)},
    {label:'Heap Objects', value:fmtNum(cur.heap?cur.heap.objects:0)},
    {label:'Heap Sys', value:fmtBytes(cur.heap?cur.heap.sys:0)},
    {label:'Stack In Use', value:fmtBytes(cur.stack?cur.stack.in_use:0)},
    {label:'GC Count', value:fmtNum(cur.gc?cur.gc.num_gc:0)},
    {label:'GC Pause (last)', value:fmtDur(cur.gc?cur.gc.pause_ns/1000:0)},
    {label:'GC Pause Total', value:fmtDur(cur.gc?cur.gc.pause_total_ns/1000:0)},
    {label:'GC CPU %', value:fmtNum(cur.gc?cur.gc.cpu_fraction*100:0, 4)+'%'},
    {label:'Next GC', value:fmtBytes(cur.gc?cur.gc.next_gc:0)},
    {label:'Mallocs', value:fmtNum(cur.heap?cur.heap.mallocs:0)},
    {label:'Frees', value:fmtNum(cur.heap?cur.heap.frees:0)},
    {label:'Total Alloc', value:fmtBytes(cur.heap?cur.heap.total_alloc:0)},
    {label:'CPU Usage (of one core)', value:fmtNum(cur.cpu?cur.cpu.usage_pct:0, 1)+'%'},
    {label:'Mem Sys (OS)', value:fmtBytes(cur.memory?cur.memory.sys:0)},
    {label:'Mem Usage %', value:fmtNum(cur.memory?cur.memory.usage_pct:0, 1)+'%'},
    {label:'CGO Calls', value:fmtNum(cur.cpu?cur.cpu.cgo_calls:0)},
    {label:'Num CPU', value:fmtNum(cur.cpu?cur.cpu.num_cpu:0)},
    {label:'GOMAXPROCS', value:fmtNum(cur.cpu?cur.cpu.gomaxprocs:0)},
    {label:'GOGC', value:rt.gogc!==undefined?(rt.gogc===-1?'disabled':String(rt.gogc)):'—', cls:rt.gogc===-1?'red':''},
    {label:'GOMEMLIMIT', value:rt.gomemlimit>0?fmtBytes(rt.gomemlimit):'no limit', cls:rt.gomemlimit<=0?'yellow':''},
  ];
  renderCards($('#perf-cards'), cards);

  // Heap Allocation chart — plots HeapAlloc ONLY (live heap bytes).
  // This DROPS after GC. Never mix with TotalAlloc or HeapSys.
  drawLineChart($('#chart-perf-heap'), hist.map(function(h){return (h.heap_alloc||0)/1024/1024;}), chartColor(3));

  // Goroutines chart
  drawLineChart($('#chart-perf-goro'), hist.map(function(h){return h.goroutines||0;}), chartColor(4));

  // CPU Usage chart
  drawLineChart($('#chart-perf-cpu'), hist.map(function(h){return h.cpu_usage||0;}), chartColor(5));

  // GC chart — plots NumGC (GC count) over time.
  // Each step up represents a GC event. This shows ACTUAL GC events
  // instead of a flat line (which is what plotting pause_ns would show,
  // since pause_ns is constant between GCs).
  drawLineChart($('#chart-perf-gc'), hist.map(function(h){return h.num_gc||0;}), chartColor(6));

  // Memory from OS chart — plots Sys (total bytes from OS).
  // This is separate from HeapAlloc (live heap). Sys grows when Go
  // requests memory from the OS and shrinks when Go returns it
  // (HeapReleased).
  drawLineChart($('#chart-perf-mem-sys'), hist.map(function(h){return (h.sys||0)/1024/1024;}), chartColor(7));

  // GC Pause chart — plots the most recent GC pause duration at each
  // sample. Shows how long the last GC took.
  drawLineChart($('#chart-perf-pause'), hist.map(function(h){return (h.pause_ns||0)/1000/1000;}), chartColor(8));
}

// ─── Timeline ──────────────────────────────────────────────────────────
function loadTimelines(signal){
  return api('timeline?limit=50', {signal:signal}).then(function(d){S.timelines=d||[]; renderTimelineList();}).catch(function(){});
}
function initTimeline(signal){
  S._tlShown = null;
  loadTimelines(signal);
  startPagePoll('timeline', loadTimelines, 10000);
}
function renderTimelineList(){
  var list = S.timelines.slice();
  var c = $('#tl-count');
  if(c) c.textContent = list.length;
  var html = '';
  list.slice(0,100).forEach(function(t){
    html += '<tr><td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(t.time)+'</td>'+
      '<td><span class="method-pill '+cls(t.method)+'">'+escapeHTML(t.method)+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(t.path)+'</td>'+
      '<td><span class="status-pill '+statusClass(t.status)+'">'+(t.status?num(t.status):'-')+'</span></td>'+
      '<td>'+fmtDur(t.total_us)+'</td><td>'+fmtNum((t.steps||[]).length)+'</td>'+
      '<td><a href="#" data-id="'+attr(t.id)+'">view</a></td></tr>';
  });
  if(!list.length) html = '<tr><td colspan="7" class="empty">No timelines yet. A timeline is recorded for a request that arrives while this dashboard is open.</td></tr>';
  var tb = $('#tl-tbody');
  if(tb) tb.innerHTML = html;
  $$('a[data-id]').forEach(function(a){
    pageOn(a, 'click', function(e){e.preventDefault(); S._tlSelected=a.dataset.id; renderTimelineDetail();});
  });
  // The detail is drawn when the selection changes, not on every list refresh: redrawing it
  // closed every step the person had opened.
  if(S._tlSelected && S._tlShown !== S._tlSelected) renderTimelineDetail();
}
// A timeline lists every step once (flat, in start order) and also nests each step's
// descendants in its children. Drawn naively that shows every nested step twice, so the page
// draws the tree from the steps that are nobody's child.
function stepKey(s){ return s.name+'|'+s.start+'|'+s.end; }
function timelineRoots(steps){
  var nested = {};
  (function mark(list){
    list.forEach(function(s){
      (s.children||[]).forEach(function(c){ nested[stepKey(c)] = true; });
      mark(s.children||[]);
    });
  })(steps);
  return steps.filter(function(s){ return !nested[stepKey(s)]; });
}
function renderTimelineDetail(){
  var d = $('#tl-detail');
  if(!d) return;
  var id = S._tlSelected;
  if(!id){ d.innerHTML = ''; S._tlShown = null; return; }
  var t = S.timelines.find(function(x){return x.id===id;});
  if(!t){
    // Reached from the Requests page, the timeline may be older than the latest fifty the list holds.
    if(S._tlFetching === id) return;
    S._tlFetching = id;
    d.innerHTML = '<div class="empty">Loading timeline\u2026</div>';
    api('timeline/'+encodeURIComponent(id), {signal:currentPageSignal()}).then(function(tl){
      S._tlFetching = null;
      if(tl && tl.id){ S.timelines.unshift(tl); }
      renderTimelineDetail();
    }).catch(function(e){
      S._tlFetching = null;
      if(e && e.name === 'AbortError') return;
      S._tlSelected = null;
      d.innerHTML = '';
    });
    return;
  }
  S._tlShown = id;
  d.innerHTML = '<div class="chart-card reveal-in"><div class="head"><h3>Timeline '+escapeHTML(t.id)+'</h3><button id="tl-close" type="button">Close</button></div>'+
    (t.dropped_steps ? '<div class="notice">This request produced '+num(t.dropped_steps)+' more steps than the recorder keeps; they are not shown.</div>' : '')+
    '<div class="timeline">'+renderTimelineSteps(timelineRoots(t.steps||[]), 0)+'</div></div>';
  var cl = $('#tl-close');
  if(cl) pageOn(cl, 'click', function(){S._tlSelected=null; S._tlShown=null; d.innerHTML='';});
  $$('#tl-detail .t-step > .head').forEach(function(h){
    var toggle = function(){
      var step = h.parentNode;
      var open = step.classList.toggle('open');
      h.setAttribute('aria-expanded', open ? 'true' : 'false');
    };
    pageOn(h, 'click', function(e){ e.stopPropagation(); toggle(); });
    pageOn(h, 'keydown', function(e){ if(e.key==='Enter' || e.key===' '){ e.preventDefault(); toggle(); } });
  });
}
function renderTimelineSteps(steps, depth){
  var html = '';
  steps.forEach(function(s){
    var slow = s.duration_us > 100000;
    html += '<div class="t-step '+(slow?'slow':'')+'" style="margin-left:'+(depth*12)+'px">'+
      '<div class="head" role="button" tabindex="0" aria-expanded="false"><span class="name">'+escapeHTML(s.name)+'</span>'+
      '<span class="dur '+(slow?'slow':'')+'">'+fmtDur(s.duration_us)+'</span></div>'+
      '<div class="expand"><div class="expand-inner">';
    if(s.metadata){
      for(var k in s.metadata){
        html += '<div><span style="color:var(--primary)">'+escapeHTML(k)+':</span> '+escapeHTML(JSON.stringify(s.metadata[k]))+'</div>';
      }
    }
    html += '<div><span style="color:var(--text-muted)">start:</span> '+fmtTime(s.start)+'</div>';
    html += '<div><span style="color:var(--text-muted)">end:</span> '+fmtTime(s.end)+'</div>';
    html += '</div></div>';
    if(s.children && s.children.length) html += '<div class="children">'+renderTimelineSteps(s.children, depth+1)+'</div>';
    html += '</div>';
  });
  return html;
}

// ─── Architecture ─────────────────────────────────────────────────────
var archZoom = 1, archPanX = 0, archPanY = 0;
var archDragging = false, archLastX = 0, archLastY = 0;
var _archGlobalListenersBound = false;

function applyArchTransform(){
  var vp = $('#arch-viewport');
  var zLabel = $('#arch-zoom-label');
  if(vp) vp.style.transform = 'translate('+archPanX+'px,'+archPanY+'px) scale('+archZoom+')';
  if(zLabel) zLabel.textContent = Math.round(archZoom*100)+'%';
}

function initArchitecture(signal){
  loadArchitecture(signal);
  startPagePoll('architecture', loadArchitecture, 5000);
  setupArchControls();
}
function loadArchitecture(signal){
  return api('architecture', {signal:signal}).then(function(d){S.arch=d; renderArchitecture();}).catch(function(){});
}

function setupArchControls(){
  var wrap = $('#arch-canvas-wrap');
  var vp = $('#arch-viewport');
  if(!wrap || !vp) return;

  // Zoom buttons
  var zIn = $('#arch-zoom-in');
  var zOut = $('#arch-zoom-out');
  var zReset = $('#arch-zoom-reset');
  var zLabel = $('#arch-zoom-label');
  if(zIn) pageOn(zIn, 'click', function(){ archZoom = Math.min(archZoom*1.2, 3); applyArchTransform(); });
  if(zOut) pageOn(zOut, 'click', function(){ archZoom = Math.max(archZoom/1.2, 0.3); applyArchTransform(); });
  if(zReset) pageOn(zReset, 'click', function(){ archZoom=1; archPanX=0; archPanY=0; applyArchTransform(); });

  // Mouse wheel zoom
  pageOn(wrap, 'wheel', function(e){
    e.preventDefault();
    var delta = e.deltaY < 0 ? 1.1 : 1/1.1;
    archZoom = Math.max(0.3, Math.min(archZoom*delta, 3));
    applyArchTransform();
  }, {passive:false});

  // Drag to pan
  pageOn(wrap, 'mousedown', function(e){
    archDragging = true;
    archLastX = e.clientX;
    archLastY = e.clientY;
    vp.classList.add('no-transition');
  });
  // The drag listeners live on document, so bind them once for the lifetime
  // of the dashboard. Rebinding on each navigation leaked closures over old
  // viewport nodes and made one mouse gesture update multiple stale views.
  if(!_archGlobalListenersBound){
    document.addEventListener('mousemove', function(e){
      if(!archDragging || S.page!=='architecture') return;
      archPanX += e.clientX - archLastX;
      archPanY += e.clientY - archLastY;
      archLastX = e.clientX;
      archLastY = e.clientY;
      applyArchTransform();
    });
    document.addEventListener('mouseup', function(){
      if(archDragging){
        archDragging = false;
        var currentVp = $('#arch-viewport');
        if(currentVp) currentVp.classList.remove('no-transition');
      }
    });
    _archGlobalListenersBound = true;
  }

  // Touch support
  pageOn(wrap, 'touchstart', function(e){
    if(e.touches.length===1){
      archDragging = true;
      archLastX = e.touches[0].clientX;
      archLastY = e.touches[0].clientY;
      if(vp) vp.classList.add('no-transition');
    }
  }, {passive:true});
  pageOn(wrap, 'touchmove', function(e){
    if(!archDragging || e.touches.length!==1) return;
    archPanX += e.touches[0].clientX - archLastX;
    archPanY += e.touches[0].clientY - archLastY;
    archLastX = e.touches[0].clientX;
    archLastY = e.touches[0].clientY;
    applyArchTransform();
  }, {passive:true});
  pageOn(wrap, 'touchend', function(){
    archDragging = false;
    if(vp) vp.classList.remove('no-transition');
  });
  applyArchTransform();
}

// archIcon returns an SVG icon for a connection. It first checks the driver
// name (postgres, redis, kafka, elasticsearch, aws, ...) and falls back to
// a type-based generic icon. If neither matches, the server icon is used.
function archIcon(type, driver){
  var d = (driver||'').toLowerCase();
  if(archDriverIcons[d]) return archDriverIcons[d];
  if(archTypeIcons[type]) return archTypeIcons[type];
  return archTypeIcons.server;
}

var archTypeIcons = {
  database: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="5" rx="8" ry="2.5"/><path d="M 4 5 L 4 19 C 4 20.5 7.6 21.5 12 21.5 C 16.4 21.5 20 20.5 20 19 L 20 5"/><path d="M 4 12 C 4 13.5 7.6 14.5 12 14.5 C 16.4 14.5 20 13.5 20 12"/></svg>',
  cache: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M 4 7 L 12 3 L 20 7 L 20 17 L 12 21 L 4 17 Z"/><path d="M 4 7 L 12 11 L 20 7 M 12 11 L 12 21"/></svg>',
  message_queue: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="6" width="18" height="12" rx="1"/><path d="M 6 10 L 10 10 M 6 14 L 9 14 M 14 10 L 18 10 M 14 14 L 17 14"/></svg>',
  search: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="7"/><path d="M 16 16 L 21 21"/></svg>',
  object_store: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M 3 7 L 12 3 L 21 7 L 21 17 L 12 21 L 3 17 Z"/><path d="M 3 7 L 12 11 L 21 7 M 12 11 L 12 21"/></svg>',
  smtp: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="M 3 7 L 12 13 L 21 7"/></svg>',
  http: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M 3 12 L 21 12 M 12 3 C 15 6 15 18 12 21 M 12 3 C 9 6 9 18 12 21"/></svg>',
  grpc: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M 12 2 L 2 7 L 12 12 L 22 7 Z"/><path d="M 2 17 L 12 22 L 22 17 M 2 12 L 12 17 L 22 12"/></svg>',
  websocket: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M 6 12 C 6 8 9 5 12 5 C 15 5 18 8 18 12"/><path d="M 9 12 C 9 10 10 9 12 9 C 14 9 15 10 15 12"/><circle cx="12" cy="12" r="1.5" fill="currentColor"/></svg>',
  cloud: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M 6 18 C 3.5 18 2 16 2 14 C 2 12 4 10 6.5 10 C 7 7 9 5 12 5 C 15 5 17 7 17.5 10 C 20 10 22 12 22 14 C 22 16 20.5 18 18 18 Z"/></svg>',
  server: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/><circle cx="7" cy="7.5" r="1" fill="currentColor"/><circle cx="7" cy="16.5" r="1" fill="currentColor"/></svg>',
  graph: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="6" r="2"/><circle cx="18" cy="6" r="2"/><circle cx="12" cy="18" r="2"/><path d="M 7.5 7.5 L 10.5 16.5 M 16.5 7.5 L 13.5 16.5 M 8 6 L 16 6"/></svg>',
  timeseries_db: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M 3 17 L 8 12 L 12 15 L 21 6"/><path d="M 3 21 L 21 21 M 3 21 L 3 3"/></svg>',
  vector_db: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="6" r="2"/><circle cx="18" cy="6" r="2"/><circle cx="12" cy="18" r="2"/><circle cx="6" cy="18" r="2"/><circle cx="18" cy="18" r="2"/><path d="M 6 8 L 6 16 M 18 8 L 18 16 M 8 6 L 16 6 M 8 18 L 16 18"/></svg>',
  cdn: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M 12 3 L 12 21 M 3 12 L 21 12 M 5.6 5.6 L 18.4 18.4 M 18.4 5.6 L 5.6 18.4"/></svg>',
  dns: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M 8 8 L 16 8 M 8 12 L 16 12 M 8 16 L 16 16 M 10 8 L 10 16 M 14 8 L 14 16"/></svg>',
  ldap: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="4"/><path d="M 4 21 C 4 16 8 14 12 14 C 16 14 20 16 20 21"/></svg>',
  custom: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/></svg>',
};

var archDriverIcons = {
  postgres: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M23.4 14.4c-.4-1.2-1.4-1.9-2.6-1.8-.4 0-.8.1-1.3.2-.8-1.8-1.8-3.3-3.1-4.8C17.5 6.9 18.4 5.6 18.9 4.2c.4-1.3.1-2.4-.9-3.2C17.3.4 16.4.1 15.4.1c-.9 0-1.9.2-2.9.6C11.5.3 10.5.1 9.5.1 8.5.1 7.6.4 6.9 1 5.9 1.8 5.6 2.9 6 4.2c.5 1.4 1.4 2.7 2.5 3.8C7.2 9.5 6.2 11 5.4 12.8c-.5-.1-.9-.2-1.3-.2C2.9 12.5 1.9 13.2 1.5 14.4c-.4 1.2 0 2.5 1 3.3.5.4 1.1.7 1.7.9-.2.8-.2 1.6-.2 2.4 0 1.7 1.3 3 3 3 1 0 2-.5 2.6-1.3.8.3 1.6.5 2.4.5s1.6-.2 2.4-.5c.6.8 1.6 1.3 2.6 1.3 1.7 0 3-1.3 3-3 0-.8 0-1.6-.2-2.4.6-.2 1.2-.5 1.7-.9 1-.8 1.4-2.1 1-3.3z"/></svg>',
  postgresql: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M23.4 14.4c-.4-1.2-1.4-1.9-2.6-1.8-.4 0-.8.1-1.3.2-.8-1.8-1.8-3.3-3.1-4.8C17.5 6.9 18.4 5.6 18.9 4.2c.4-1.3.1-2.4-.9-3.2C17.3.4 16.4.1 15.4.1c-.9 0-1.9.2-2.9.6C11.5.3 10.5.1 9.5.1 8.5.1 7.6.4 6.9 1 5.9 1.8 5.6 2.9 6 4.2c.5 1.4 1.4 2.7 2.5 3.8C7.2 9.5 6.2 11 5.4 12.8c-.5-.1-.9-.2-1.3-.2C2.9 12.5 1.9 13.2 1.5 14.4c-.4 1.2 0 2.5 1 3.3.5.4 1.1.7 1.7.9-.2.8-.2 1.6-.2 2.4 0 1.7 1.3 3 3 3 1 0 2-.5 2.6-1.3.8.3 1.6.5 2.4.5s1.6-.2 2.4-.5c.6.8 1.6 1.3 2.6 1.3 1.7 0 3-1.3 3-3 0-.8 0-1.6-.2-2.4.6-.2 1.2-.5 1.7-.9 1-.8 1.4-2.1 1-3.3z"/></svg>',
  mysql: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M5.4 21.3c-.6 0-1.1.2-1.6.5L3 21.2c.2-.5.4-.9.5-1.4.2-1 0-2.1-.5-3-.6-1-1.5-1.8-2.5-2.2v-.4c1.3-.2 2.5-.7 3.5-1.6 1-1 1.5-2.3 1.5-3.7 0-1.4-.5-2.8-1.5-3.9C3 4 1.8 3.4.5 3.2v-.4c1 0 2-.2 2.9-.6C4.4 1.8 5.2 1.1 5.8.3h.7c.7 1.2 1.9 2.1 3.3 2.4 1.4.3 2.9.1 4.2-.6l.5.4c-.7 1-1.1 2.2-1.1 3.5 0 1.3.4 2.5 1.2 3.5.8 1 1.9 1.7 3.2 2v.5c-1.3.3-2.4 1-3.2 2-.8 1-1.2 2.3-1.2 3.5 0 1.3.4 2.5 1.2 3.5l-.6.5c-1.5-1-3.3-1.4-5-1.2-1.5.2-2.8.8-3.6 1z"/></svg>',
  redis: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M10.5 12c-.3 0-.6.1-.9.2-.3.1-.6.3-.9.4-.3.2-.6.3-.9.4-.3.1-.6.2-.9.2-.3 0-.6-.1-.9-.2-.3-.1-.6-.2-.9-.4-.3-.2-.6-.3-.9-.4-.3-.1-.6-.2-.9-.2-.3 0-.6.1-.9.2-.3.1-.6.3-.9.4-.3.2-.6.3-.9.4-.3.1-.6.2-.9.2v2c.5 0 1-.2 1.5-.4.3-.2.6-.3.9-.4.3-.1.6-.2.9-.2.3 0 .6.1.9.2.3.1.6.2.9.4.3.2.6.3.9.4.3.1.6.2.9.2.3 0 .6-.1.9-.2.3-.1.6-.3.9-.4.3-.2.6-.3.9-.4.3-.1.6-.2.9-.2.3 0 .6.1.9.2.3.1.6.3.9.4.3.2.6.3.9.4.3.1.6.2.9.2V13s-.3-.1-.6-.2c-.3-.1-.6-.3-.9-.4-.3-.2-.6-.3-.9-.4-.3-.1-.6-.2-.9-.2z M1 17c.5 0 1-.2 1.5-.4.3-.2.6-.3.9-.4.3-.1.6-.2.9-.2.3 0 .6.1.9.2.3.1.6.2.9.4.3.2.6.3.9.4.3.1.6.2.9.2.3 0 .6-.1.9-.2.3-.1.6-.3.9-.4.3-.2.6-.3.9-.4.3-.1.6-.2.9-.2.3 0 .6.1.9.2.3.1.6.3.9.4.3.2.6.3.9.4.3.1.6.2.9.2v-2c-.5 0-1-.2-1.5-.4-.3-.2-.6-.3-.9-.4-.3-.1-.6-.2-.9-.2-.3 0-.6.1-.9.2-.3.1-.6.3-.9.4-.3.2-.6.3-.9.4-.3.1-.6.2-.9.2-.3 0-.6-.1-.9-.2-.3-.1-.6-.2-.9-.4-.3-.2-.6-.3-.9-.4-.3-.1-.6-.2-.9-.2-.3 0-.6.1-.9.2-.3.1-.6.3-.9.4-.3.2-.6.3-.9.4-.3.1-.6.2-.9.2V17z M1 22c.5 0 1-.2 1.5-.4.3-.2.6-.3.9-.4.3-.1.6-.2.9-.2.3 0 .6.1.9.2.3.1.6.2.9.4.3.2.6.3.9.4.3.1.6.2.9.2.3 0 .6-.1.9-.2.3-.1.6-.3.9-.4.3-.2.6-.3.9-.4.3-.1.6-.2.9-.2.3 0 .6.1.9.2.3.1.6.3.9.4.3.2.6.3.9.4.3.1.6.2.9.2v-2c-.5 0-1-.2-1.5-.4-.3-.2-.6-.3-.9-.4-.3-.1-.6-.2-.9-.2-.3 0-.6.1-.9.2-.3.1-.6.2-.9.4-.3.2-.6.3-.9.4-.3.1-.6.2-.9.2-.3 0-.6-.1-.9-.2-.3-.1-.6-.2-.9-.4-.3-.2-.6-.3-.9-.4-.3-.1-.6-.2-.9-.2-.3 0-.6.1-.9.2-.3.1-.6.3-.9.4-.3.2-.6.3-.9.4-.3.1-.6.2-.9.2V22z M12 0C9.2 0 7 2.2 7 5v2h10V5c0-2.8-2.2-5-5-5z"/></svg>',
  rabbitmq: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M23 9h-7c-.6 0-1-.4-1-1V1c0-.6-.4-1-1-1h-4c-.6 0-1 .4-1 1v7c0 .6-.4 1-1 1H1c-.6 0-1 .4-1 1v4c0 .6.4 1 1 1h7c.6 0 1 .4 1 1v7c0 .6.4 1 1 1h4c.6 0 1-.4 1-1v-7c0-.6.4-1 1-1h7c.6 0 1-.4 1-1v-4c0-.6-.4-1-1-1z"/></svg>',
  kafka: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.6 0 12 0zm-3 16.5v-9h2v3.5h2V7.5h2v9h-2V13h-2v3.5H9z"/></svg>',
  elasticsearch: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M2 13s0 1 .5 2 1.5 2 3 2.5 3.5.5 6.5.5h8c.5 0 1 .5 1 1s-.5 1-1 1H11c-3.5 0-6-.5-7.5-1.5S1.5 16 1.5 14v-1H2zm-.5-3s0-1 1-2 2.5-2 5.5-2h11c.5 0 1-.5 1-1s-.5-1-1-1H8.5C5 2 3 3 2 4s-1 2-1 2.5V8h.5zm11.5-8c0 .5-.5 1-1 1H7c-.5 0-1-.5-1-1s.5-1 1-1h5c.5 0 1 .5 1 1zm0 20c0 .5-.5 1-1 1H8c-.5 0-1-.5-1-1s.5-1 1-1h4c.5 0 1 .5 1 1z"/></svg>',
  mongodb: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0c-.3 0-.5.2-.6.5C11 2 10.2 4 9 5.6 7.5 7.6 6 9.5 6 12.5c0 3.5 2.5 6.5 6 7V24h1v-4.5c3.5-.5 6-3.5 6-7 0-3-1.5-4.9-3-6.9C12.8 4 12 2 11.6.5 11.5.2 11.3 0 11 0H12z"/></svg>',
  aws: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M6 13c0 .3.1.5.3.6.2.1.5.2.8.2.3 0 .6-.1.8-.3.2-.2.3-.4.3-.7 0-.3-.1-.5-.3-.6-.2-.1-.5-.2-.9-.2H6v-1h1c.3 0 .5 0 .6-.1.2-.1.3-.3.3-.5 0-.2-.1-.4-.3-.5-.1-.1-.3-.2-.6-.2-.3 0-.5.1-.7.2-.2.1-.3.3-.3.5H5c0-.5.2-.9.6-1.2.4-.3.9-.4 1.5-.4.6 0 1.1.1 1.5.4.4.3.6.7.6 1.2 0 .3-.1.6-.3.8-.2.2-.4.4-.7.5.3.1.6.3.8.5.2.3.3.6.3.9 0 .5-.2.9-.7 1.3-.4.3-1 .5-1.6.5-.6 0-1.1-.2-1.5-.5-.4-.4-.6-.8-.6-1.3H6zm5-4v6h1V9h-1zm3 0v6h2v-5h2V9h-4zm8 5c-.5.7-2 2-5 3-3 1-7 1-7 1s-4 0-6-1c-2-1-2.5-2.3-3-3-.2-.3 0-.5.3-.4 2.7.9 6.7 1.4 10.7 1.4s8-.5 10.7-1.4c.3-.1.5.1.3.4z"/></svg>',
  gcp: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M16 2l-4 4 4 4 4-4-4-4zM8 4L4 8v8l4 4h4l4-4v-4h-4v4H8V8h4V4H8z"/></svg>',
  azure: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M5 21l8-10-4-10H5v20zm9-18l4 13V3h-4zM13 21h6l-6-14v14z"/></svg>',
  docker: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M22 10c-.5-.5-1.5-.7-2.3-.5-.1-.8-.5-1.5-1.2-2.1l-.5-.4-.4.5c-.4.6-.6 1.5-.5 2.3.1.4.2.7.5 1-.4.2-1.1.5-2.1.5H.6c-.1 1.2-.1 4.2 1.9 6.5C3.9 20.3 6.9 21 10.4 21c7.5 0 12.5-4.5 12.8-10.5.7 0 1.5-.5 1.8-1zM2 9h3V6H2v3zm4 0h3V6H6v3zm4 0h3V6h-3v3zm0-4h3V2h-3v3zM6 5h3V2H6v3z"/></svg>',
  kubernetes: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0L2 4.5.5 15.5 7.5 23h9l7-7.5L22 4.5 12 0zm0 3l7.5 4 1 7.5L15 20H9l-5.5-5.5 1-7.5L12 3z"/></svg>',
  cloudflare: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M16.3 9.6c-.3-.1-.6-.1-.9-.1H7c-.2 0-.3.1-.4.3-1.8 2.6-1.2 6.2 1.4 8 .9.6 2 .9 3 .9h7.3c.5 0 1-.1 1.5-.3 1-.5 1.7-1.4 1.9-2.5.3-1.4-.2-2.8-1.3-3.7-.8-.6-1.8-.9-2.8-.9z"/></svg>',
  minio: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.6 0 12 0zm0 2c5.5 0 10 4.5 10 10s-4.5 10-10 10S2 17.5 2 12 6.5 2 12 2zm0 3l-5 7 5 7 5-7-5-7z"/></svg>',
  s3: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0L0 6v12l12 6 12-6V6L12 0zm0 3l9 4.5v9L12 21l-9-4.5v-9L12 3z"/></svg>',
  neo4j: '<svg viewBox="0 0 24 24" fill="currentColor"><circle cx="12" cy="12" r="4"/><circle cx="5" cy="5" r="3"/><circle cx="19" cy="5" r="3"/><circle cx="5" cy="19" r="3"/><circle cx="19" cy="19" r="3"/></svg>',
  influxdb: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M21.4 4H2.6c-.3 0-.6.3-.6.6v14.8c0 .3.3.6.6.6h18.8c.3 0 .6-.3.6-.6V4.6c0-.3-.3-.6-.6-.6zM19 17H5V7h14v10z"/></svg>',
  prometheus: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.6 0 12 0zm0 22C6.5 22 2 17.5 2 12S6.5 2 12 2s10 4.5 10 10-4.5 10-10 10zm0-17c-3.9 0-7 3.1-7 7s3.1 7 7 7 7-3.1 7-7-3.1-7-7-7zm2 9h-4V8h4v6z"/></svg>',
  nats: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0L0 12l12 12 12-12L12 0zm0 3l9 9-9 9-9-9 9-9z"/></svg>',
  memcached: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M3 8c-.6 0-1 .4-1 1v2c0 .6.4 1 1 1h18c.6 0 1-.4 1-1V9c0-.6-.4-1-1-1H3zm2 6c-.6 0-1 .4-1 1v2c0 .6.4 1 1 1h14c.6 0 1-.4 1-1v-2c0-.6-.4-1-1-1H5zm2 6c-.6 0-1 .4-1 1v2c0 .6.4 1 1 1h10c.6 0 1-.4 1-1v-2c0-.6-.4-1-1-1H7zM12 0C9.2 0 7 2.2 7 5v2h10V5c0-2.8-2.2-5-5-5z"/></svg>',
  mariadb: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M1.5 18.5c-.5-.2-.8-.6-.8-1.1 0-.6.3-1.1.8-1.3.5-.2 1.1-.2 1.7 0 .6.2 1.1.6 1.4 1.1.4.7 1 1.2 1.7 1.4.7.2 1.5.1 2.1-.3.6-.4 1-1.1 1-1.8 0-.8-.4-1.5-1.1-1.9-.8-.4-1.6-.6-2.5-.6-1.3 0-2.5-.5-3.4-1.4C.9 11.7.4 10.5.4 9.2S.9 6.7 1.8 5.8c.9-.9 2.1-1.4 3.4-1.4 1.2 0 2.4.5 3.3 1.3l.4-.3c-.5-.6-.8-1.4-.8-2.2 0-.9.3-1.7 1-2.3C9.7.3 10.5 0 11.4 0c.9 0 1.7.3 2.4.9.7.6 1 1.4 1 2.3 0 .8-.3 1.6-.8 2.2l.4.3c.9-.8 2.1-1.3 3.3-1.3 1.3 0 2.5.5 3.4 1.4.9.9 1.4 2.1 1.4 3.4s-.5 2.5-1.4 3.4c-.9.9-2.1 1.4-3.4 1.4-.9 0-1.7.2-2.5.6-.7.4-1.1 1.1-1.1 1.9 0 .7.4 1.4 1 1.8.6.4 1.4.5 2.1.3.7-.2 1.3-.7 1.7-1.4.3-.5.8-.9 1.4-1.1.6-.2 1.2-.2 1.7 0 .5.2.8.7.8 1.3 0 .5-.3.9-.8 1.1-1.5.6-3.1.9-4.7.9-1.8 0-3.5-.4-5.1-1.2-1.7.8-3.5 1.2-5.4 1.2-2 0-3.9-.3-5.8-.9z"/></svg>',
  sqlite: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.6 0 12 0zm0 2c5.5 0 10 4.5 10 10s-4.5 10-10 10S2 17.5 2 12 6.5 2 12 2zm0 4c-3.3 0-6 2.7-6 6s2.7 6 6 6 6-2.7 6-6-2.7-6-6-6z"/></svg>',
  sendgrid: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M0 0v24h24V0H0zm8 8h8v8H8V8z"/></svg>',
  mailgun: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.6 0 12 0zm5 16h-3l-4-6v6H7V8h3l4 6V8h3v8z"/></svg>',
  ses: '<svg viewBox="0 0 24 24" fill="currentColor"><rect x="2" y="5" width="20" height="14" rx="2"/><path d="M2 7l10 6 10-6" stroke="#000" stroke-width="1.5" fill="none"/></svg>',
  opensearch: '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.6 0 12 0zm5 16h-3l-4-6v6H7V8h3l4 6V8h3v8z"/></svg>',
};

function renderArchitecture(){
  var d = S.arch || {};
  var conns = d.connections || [];

  // Summary
  var summary = $('#arch-summary');
  if(summary){
    summary.innerHTML =
      '<div class="arch-stat green"><div><div class="num">'+(d.connected||0)+'</div><div class="lbl">Connected</div></div></div>'+
      '<div class="arch-stat yellow"><div><div class="num">'+(d.degraded||0)+'</div><div class="lbl">Degraded</div></div></div>'+
      '<div class="arch-stat red"><div><div class="num">'+(d.disconnected||0)+'</div><div class="lbl">Disconnected</div></div></div>'+
      '<div class="arch-stat gray"><div><div class="num">'+(d.total||0)+'</div><div class="lbl">Total Services</div></div></div>';
  }

  // SVG: only lines + center node (icons are HTML overlay)
  var svg = $('#arch-svg');
  var overlay = $('#arch-overlay');
  if(svg && overlay){
    var cx = 450, cy = 350;
    var radius = 200;
    var n = conns.length;
    var linesHtml = '';

    // Center node
    linesHtml += '<circle class="center-node" cx="'+cx+'" cy="'+cy+'" r="36"/>';
    linesHtml += '<text class="center-label" x="'+cx+'" y="'+cy+'">Breeze</text>';

    for(var i=0; i<n; i++){
      var angle = (i / Math.max(n,1)) * 2 * Math.PI - Math.PI/2;
      var x = cx + Math.cos(angle) * radius;
      var y = cy + Math.sin(angle) * radius;

      var cls = connState(conns[i].status);
      // Line stops short of the icon (icon is ~48px, so stop 28px before)
      var lineEndX = cx + Math.cos(angle) * (radius - 28);
      var lineEndY = cy + Math.sin(angle) * (radius - 28);
      linesHtml += '<line class="conn-line '+cls+'" x1="'+cx+'" y1="'+cy+'" x2="'+lineEndX+'" y2="'+lineEndY+'"/>';
      if(cls === 'connected'){
        linesHtml += '<circle class="conn-pulse connected" cx="'+lineEndX+'" cy="'+lineEndY+'" r="4"/>';
      }
    }
    svg.innerHTML = linesHtml;

    // HTML overlay: icons + labels at endpoints
    var overlayHtml = '';
    for(var i=0; i<n; i++){
      var angle = (i / Math.max(n,1)) * 2 * Math.PI - Math.PI/2;
      // Convert SVG coords to percentage for HTML overlay
      var xPct = ((cx + Math.cos(angle) * radius) / 900) * 100;
      var yPct = ((cy + Math.sin(angle) * radius) / 700) * 100;

      var conn = conns[i];
      var cls = connState(conn.status);
      var hostLabel = conn.host || conn.database || '';

      overlayHtml += '<div class="arch-node '+cls+'" style="left:'+xPct+'%;top:'+yPct+'%">'+
        '<div class="arch-node-icon">'+
          '<div class="halo"></div>'+
          archIcon(conn.type, conn.driver)+
          '<div class="status-dot"></div>'+
        '</div>'+
        '<div class="arch-node-name">'+escapeHTML(conn.name)+'</div>'+
        (hostLabel?'<div class="arch-node-host">'+escapeHTML(hostLabel)+'</div>':'')+
      '</div>';
    }
    overlay.innerHTML = overlayHtml;
  }

  // Cards
  var cards = $('#arch-cards');
  if(!cards) return;
  if(!conns.length){
    cards.innerHTML = '<div class="empty" style="grid-column:1/-1"><div class="icon">&#x1F5C2;</div>No external connections registered. Call <code style="background:var(--bg);padding:2px 6px;border-radius:3px">coll.RegisterConnection(...)</code> to add services.</div>';
    return;
  }
  var html = '';
  conns.forEach(function(c){
    var cls = connState(c.status);
    var poolHtml = '';
    if(c.pool_max > 0){
      var pct = Math.round(c.pool_in_use / c.pool_max * 100);
      var poolCls = pct > 80 ? 'high' : pct > 50 ? 'med' : '';
      poolHtml = '<div class="row"><span>Pool</span><span class="val">'+c.pool_in_use+'/'+c.pool_max+'</span></div>'+
        '<div class="pool-bar"><div class="fill '+poolCls+'" style="width:'+pct+'%"></div></div>';
    }
    var detailsHtml = '';
    if(c.details){
      for(var k in c.details){
        detailsHtml += '<div class="row"><span>'+escapeHTML(k)+'</span><span class="val">'+escapeHTML(c.details[k])+'</span></div>';
      }
    }
    var typeLabel = c.driver || c.type || 'service';
    html += '<div class="arch-card '+cls+'">'+
      '<div class="head">'+
        '<div class="icon-wrap">'+archIcon(c.type, c.driver)+'</div>'+
        '<div class="title"><div class="name">'+escapeHTML(c.name)+'</div><div class="driver">'+escapeHTML(typeLabel)+'</div></div>'+
        '<span class="status-pill">'+escapeHTML(c.status||'unknown')+'</span>'+
      '</div>'+
      '<div class="body">'+
        (c.host?'<div class="row"><span>Host</span><span class="val">'+escapeHTML(c.host)+'</span></div>':'')+
        (c.database?'<div class="row"><span>DB</span><span class="val">'+escapeHTML(c.database)+'</span></div>':'')+
        (c.latency_ms?'<div class="row"><span>Latency</span><span class="val">'+fmtMS(c.latency_ms)+'</span></div>':'')+
        (c.message?'<div class="row"><span>Status</span><span class="val">'+escapeHTML(c.message)+'</span></div>':'')+
        (c.last_checked?'<div class="row"><span>Last checked</span><span class="val">'+fmtTime(c.last_checked)+'</span></div>':'')+
        poolHtml+
        detailsHtml+
      '</div>'+
    '</div>';
  });
  cards.innerHTML = html;
}

// ─── Events ────────────────────────────────────────────────────────────

// isWorkflow reports whether a row describes a workflow execution rather
// than a single event dispatch. Workflows are the one source whose spans
// are ordered steps with levels, so they get the timeline treatment.
function isWorkflow(row){ return row && row.source === 'workflow'; }

// stepStateOf reduces a finished span to one of the states the chain

// renders. Terminal rows never produce 'running': a stored span is by
// definition finished, so a spinner there would be a lie.
function stepStateOf(sp){
  if(sp.panicked) return 'panic';
  if(sp.failed)   return 'failed';
  if(sp.stopped)  return 'stopped';
  if(sp.skipped)  return 'skipped';
  return 'done';
}

// STEP_GLYPH maps a state to its marker. Shape carries the meaning
// alongside colour, so the chain stays readable to a colour-blind user
// and in a screenshot printed in greyscale.
var STEP_GLYPH = {
  done:      '\u2713',  // check
  failed:    '\u2715',  // cross
  panic:     '\u26A1',  // bolt
  stopped:   '\u25A0',  // square
  skipped:   '\u2013',  // dash
  running:   '',        // filled by a CSS spinner
  pending:   '',
  retrying:  '\u21BB',  // open-circle arrow
  compensated:'\u21A9'  // hook arrow: undone
};

// renderStepChain draws steps as connected nodes rather than bars.
//
// A workflow is a sequence of stages, and the question asked of it is
// "where did it get to", not "which stage was slowest". Nodes joined by
// arrows answer that at a glance; the duration stays on the node for
// when the follow-up question is about cost. Bars answer the second
// question first, which is why they read poorly here.
function renderStepChain(steps, opts){
  opts = opts || {};
  if(!steps || !steps.length) return '';

  var html = '<div class="wf-chain'+(opts.compensating?' compensating':'')+'">';
  steps.forEach(function(st, i){
    var state = st.state || 'done';
    var glyph = STEP_GLYPH[state] || '';
    var dur = st.duration_ms ? fmtMS(st.duration_ms) : '';
    var attempt = (st.attempt && st.attempt > 1) ? '<span class="try">#'+st.attempt+'</span>' : '';

    if(i > 0){
      // The connector takes the colour of the state it flows *into*, so
      // the eye follows the run to the point it stopped.
      html += '<div class="wf-arrow '+state+'"><i></i></div>';
    }
    html += '<div class="wf-node '+state+'" title="'+escapeHTML(st.name+(st.error?(' — '+st.error):''))+'">'+
      '<div class="dot"><span class="glyph">'+glyph+'</span></div>'+
      '<div class="body">'+
        '<div class="nm">'+escapeHTML(st.name)+'</div>'+
        '<div class="sub">'+
          '<span class="st">'+escapeHTML(state)+'</span>'+
          (dur?'<span class="dur">'+dur+'</span>':'')+
          attempt+
        '</div>'+
      '</div>'+
    '</div>';
  });
  html += '</div>';

  // Errors go under the chain rather than inside a node, so a long
  // message cannot stretch the row and break the alignment.
  var errs = steps.filter(function(s){return s.error;});
  if(errs.length){
    html += '<div class="wf-errs">';
    errs.forEach(function(s){
      html += '<div class="wf-err"><b>'+escapeHTML(s.name)+'</b>'+escapeHTML(s.error)+'</div>';
    });
    html += '</div>';
  }
  return html;
}

// renderLiveWorkflows draws the executions that have not finished.
function renderLiveWorkflows(){
  var wrap = $('#ev-live-wrap');
  var host = $('#ev-live');
  if(!wrap || !host) return;

  var live = (S.eventsMeta && S.eventsMeta.live) || [];
  if(!live.length){ wrap.style.display='none'; host.innerHTML=''; return; }

  wrap.style.display = '';
  var cnt = $('#ev-live-count');
  if(cnt) cnt.textContent = live.length;

  var html = '';
  live.forEach(function(ex){
    var steps = ex.steps || [];
    var total = steps.length;
    var done = steps.filter(function(s){return s.state==='done';}).length;
    var failed = steps.some(function(s){return s.state==='failed';});
    var running = steps.some(function(s){return s.state==='running'||s.state==='retrying';});
    var cls = failed ? 'red' : (ex.compensating ? 'yellow' : (running ? 'blue' : 'green'));
    var label = failed ? 'failed' : (ex.compensating ? 'compensating' : (running ? 'running' : (ex.done?'finished':'queued')));

    html += '<div class="wf-exec">'+
      '<div class="wf-exec-head">'+
        '<span class="nm">'+escapeHTML(ex.workflow)+'</span>'+
        '<span class="badge '+cls+'">'+label+'</span>'+
        '<span class="prog">'+done+' / '+total+' steps</span>'+
        (ex.trigger?'<span class="trg">via '+escapeHTML(ex.trigger)+'</span>':'')+
        '<span class="eid">'+escapeHTML(ex.execution_id||'')+'</span>'+
      '</div>'+
      renderStepChain(steps, {compensating: ex.compensating})+
    '</div>';
  });
  host.innerHTML = html;
}

function renderSpanTimeline(target){
  var spans = target.spans || [];
  if(!spans.length) return '';

  // A workflow reads as a chain of stages; a plain event dispatch reads
  // as a list of listeners competing for time. Those are different
  // questions, so they get different renderings rather than one
  // compromise that serves neither.
  if(isWorkflow(target)){
    return renderStepChain(spans.map(function(sp){
      return {
        name: sp.name,
        state: stepStateOf(sp),
        duration_ms: sp.duration_ms,
        error: sp.error
      };
    }), {});
  }

  var maxMS = 0;
  spans.forEach(function(sp){ if(sp.duration_ms > maxMS) maxMS = sp.duration_ms; });
  if(maxMS <= 0) maxMS = 1;


  // Group by phase, preserving first-seen order. Non-workflow sources
  // have a single phase, so they render as one flat group exactly as
  // before.
  var order = [], groups = {};
  spans.forEach(function(sp){
    var key = sp.phase || 'normal';
    if(!groups[key]){ groups[key] = []; order.push(key); }
    groups[key].push(sp);
  });

  var wf = isWorkflow(target);
  var html = '', idx = 0;
  order.forEach(function(key){
    var group = groups[key];
    var parallel = wf && group.length > 1;
    if(wf){
      // The level header states the concurrency explicitly, so the user
      // does not have to infer it from overlapping durations.
      var levelMS = 0, sumMS = 0;
      group.forEach(function(sp){
        if(sp.duration_ms > levelMS) levelMS = sp.duration_ms;
        sumMS += sp.duration_ms;
      });
      html += '<div class="wf-level'+(parallel?' parallel':'')+'">'+
        '<span class="lvl">'+escapeHTML(key)+'</span>'+
        (parallel
          ? '<span class="tag">'+group.length+' in parallel \u00b7 '+fmtMS(levelMS)+
            ' wall vs '+fmtMS(sumMS)+' total</span>'
          : '<span class="tag">sequential \u00b7 '+fmtMS(levelMS)+'</span>')+
        '</div>';
    }
    group.forEach(function(sp){
      idx++;
      var scls = sp.failed?'red':(sp.panicked?'red':(sp.stopped?'yellow':(sp.skipped?'gray':'green')));
      var pct = Math.max(1, (sp.duration_ms / maxMS) * 100);
      html += '<div class="ev-span'+(parallel?' par':'')+'">'+
        '<span class="idx">'+idx+'</span>'+
        '<span class="name">'+(parallel?'\u251c\u2500 ':'')+escapeHTML(sp.name)+'</span>'+
        (wf?'':'<span class="phase">'+escapeHTML(sp.phase||'normal')+'</span>')+
        (wf?'':'<span class="prio">p'+sp.priority+'</span>')+
        '<span class="bar"><i class="'+scls+'" style="width:'+pct.toFixed(1)+'%"></i></span>'+
        '<span class="dur">'+fmtMS(sp.duration_ms)+'</span>'+
        '<span class="badge '+scls+'">'+(sp.failed?'failed':(sp.panicked?'panic':(sp.stopped?'stopped':(sp.skipped?'skipped':'ok'))))+'</span>'+
        (sp.error?'<span class="err">'+escapeHTML(sp.error)+'</span>':'')+
        '</div>';
    });
  });
  return html;
}

function initEvents(signal){
  loadEvents(signal);
  var s = $('#ev-search');
  if(s) pageOn(s, 'input', function(){S.eventFilter.q=s.value; renderEvents(); s.focus();});
  var fo = $('#ev-failed-only');
  if(fo) pageOn(fo, 'change', function(){S.eventFilter.failedOnly=fo.checked; renderEvents();});
  var src = $('#ev-source');
  // The source filter is applied server-side, so switching it reloads
  // rather than filtering the page's current slice: the newest 200
  // workflow executions are not necessarily in the newest 200 signals.
  if(src) pageOn(src, 'change', function(){S.eventFilter.source=src.value; loadEvents(currentPageSignal());});
  startPagePoll('events', loadEvents, 10000);
}
function loadEvents(signal){
  var src = S.eventFilter.source||'';
  return api('events?limit=200'+(src?'&source='+encodeURIComponent(src):''), {signal:signal}).then(function(d){
    S.eventsMeta = d;
    // Merge: keep live WS rows that are newer than the API snapshot, then
    // prepend the snapshot's rows. WS rows carry the same shape, so a
    // dedupe by id keeps the list stable while filtering.
    var byId = {};
    (d.recent||[]).forEach(function(r){byId[r.id]=r;});
    // Live WebSocket rows are unfiltered, so drop the ones the active
    // source filter excludes; otherwise a filtered view would slowly
    // refill with other sources as events streamed in.
    S.events.forEach(function(r){
      if(src && r.source !== src) return;
      if(!byId[r.id]) byId[r.id]=r;
    });
    var merged = [];
    for(var k in byId) merged.push(byId[k]);
    merged.sort(function(a,b){return b.id-a.id;});
    S.events = merged.slice(0,500);
    renderEvents();
  }).catch(function(){});
}
function renderEvents(){
  var meta = S.eventsMeta;
  var tc = $('#ev-totals');
  if(tc){
    var t = (meta&&meta.totals)||{};
    tc.innerHTML =
      '<div class="card"><div class="label">Dispatches</div><div class="value">'+fmtNum(t.signals)+'</div></div>'+
      '<div class="card"><div class="label">Listeners Run</div><div class="value">'+fmtNum(t.listeners)+'</div></div>'+
      '<div class="card"><div class="label">Failed</div><div class="value" style="color:var(--err)">'+fmtNum(t.failed)+'</div></div>'+
      '<div class="card"><div class="label">Distinct Events</div><div class="value">'+fmtNum(t.distinct_events)+'</div></div>'+
      '<div class="card"><div class="label">Rate / sec</div><div class="value">'+fmtNum(t.rate_per_sec,1)+'</div></div>'+
      '<div class="card"><div class="label">Dropped</div><div class="value" style="color:var(--yellow)">'+fmtNum(t.dropped)+'</div></div>';
  }

  var attached = meta ? !!meta.attached : true;
  var na = $('#ev-not-attached');
  if(na){
    if(attached) na.style.display='none';
    else na.style.display='block';
  }

  renderLiveWorkflows();


  var list = S.events.slice();
  var q = (S.eventFilter.q||'').toLowerCase();
  if(q) list = list.filter(function(e){return (e.name||'').toLowerCase().indexOf(q)>=0 || (e.source||'').toLowerCase().indexOf(q)>=0 || (e.error||'').toLowerCase().indexOf(q)>=0;});
  if(S.eventFilter.failedOnly) list = list.filter(function(e){return e.failed;});
  var c = $('#ev-count');
  if(c) c.textContent = list.length;

  var html = '';
  list.slice(0,200).forEach(function(e){
    var cls = e.failed ? 'red' : (e.cancelled ? 'yellow' : (e.async ? 'blue' : 'green'));
    html += '<tr data-id="'+e.id+'">'+
      '<td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(e.time)+'</td>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:280px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(e.name)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+escapeHTML(e.source||'-')+'</td>'+
      '<td>'+fmtMS(e.duration_ms)+'</td>'+
      '<td>'+fmtNum(e.listeners)+'</td>'+
      '<td><span class="badge '+cls+'">'+(e.failed?'failed':(e.cancelled?'cancelled':(e.async?'async':'ok')))+'</span></td>'+
      '<td style="font-size:11px;color:var(--err);max-width:200px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(e.error||'')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim);max-width:160px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(e.payload||'')+'</td>'+
      '</tr>';
  });
  if(!list.length) html = '<tr><td colspan="8" class="empty">No events'+(attached?' yet':' — attach an event bus first')+'</td></tr>';
  var tb = $('#ev-tbody');
  if(tb){
    tb.innerHTML = html;
    // Clicking a row opens the per-listener breakdown below the table.
    $$('#ev-tbody tr[data-id]').forEach(function(tr){
      tr.style.cursor = 'pointer';
      pageOn(tr, 'click', function(){
        var id = Number(tr.getAttribute('data-id'));
        S.eventSel = (S.eventSel===id) ? null : id;
        renderEvents();
      });
    });
  }


  // ── Detail pane for the selected dispatch ──
  var detail = $('#ev-detail');
  if(detail){
    var sel = S.eventSel;
    var target = sel!=null ? S.events.find(function(e){return e.id===sel;}) : null;
    if(target){
      var dhtml = '<div class="chart-card"><div class="head"><h3>'+escapeHTML(target.name)+'</h3><button id="ev-close">Close</button></div>'+
        '<div class="ev-meta">'+
          '<div><span>ID</span>'+target.id+'</div>'+
          '<div><span>Source</span>'+escapeHTML(target.source||'-')+'</div>'+
          '<div><span>Started</span>'+fmtTime(target.time)+'</div>'+
          '<div><span>Duration</span>'+fmtMS(target.duration_ms)+'</div>'+
          '<div><span>'+(isWorkflow(target)?'Steps':'Listeners')+'</span>'+fmtNum(target.listeners)+'</div>'+
          (target.execution_id?'<div><span>Execution</span>'+escapeHTML(target.execution_id)+'</div>':'')+
          (target.state?'<div><span>State</span><b class="wf-state '+escapeHTML(target.state)+'">'+escapeHTML(target.state)+'</b></div>':'')+
          (target.trigger?'<div><span>Trigger</span>'+escapeHTML(target.trigger)+'</div>':'')+
          (target.request_id?'<div><span>Request</span>'+escapeHTML(target.request_id)+'</div>':'')+
        '</div>'+
        (target.error?'<div class="ev-payload"><span>error</span><pre>'+escapeHTML(target.error)+'</pre></div>':'')+
        (target.payload?'<div class="ev-payload"><span>payload</span><pre>'+escapeHTML(target.payload)+'</pre></div>':'')+
        '<div class="ev-spans"><div class="head2">'+(isWorkflow(target)?'Execution timeline':'Execution order')+'</div>';
      dhtml += renderSpanTimeline(target);
      if(!target.spans || !target.spans.length) dhtml += '<div class="empty">No step detail</div>';
      dhtml += '</div></div>';
      detail.innerHTML = dhtml;
      var cl = $('#ev-close');
      if(cl) pageOn(cl, 'click', function(){S.eventSel=null; detail.innerHTML='';});
    } else {
      detail.innerHTML = '';
    }
  }

  // ── Top events table ──
  var mrows = (meta&&meta.metrics)||[];
  var mhtml = '';
  mrows.slice(0,20).forEach(function(m){
    var fcls = m.failure_rate>0.05?'red':(m.failure_rate>0?'yellow':'green');
    mhtml += '<tr><td style="font-family:var(--mono);font-size:11px">'+escapeHTML(m.name)+'</td>'+
      '<td>'+fmtNum(m.count)+'</td>'+
      '<td>'+fmtNum(m.executed)+'</td>'+
      '<td>'+fmtNum(m.failed)+'</td>'+
      '<td>'+fmtMS(m.avg_ms)+'</td>'+
      '<td>'+fmtMS(m.max_ms)+'</td>'+
      '<td><span class="badge '+fcls+'">'+fmtNum(m.failure_rate*100,1)+'%</span></td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(m.last?fmtDate(m.last):'-')+'</td></tr>';
  });
  if(!mrows.length) mhtml = '<tr><td colspan="8" class="empty">No metrics yet</td></tr>';
  var mtb = $('#ev-metrics-tbody');
  if(mtb) mtb.innerHTML = mhtml;
}
// ─── Video Streaming ───────────────────────────────────────────────────

// The video page groups by file rather than by request. One viewer
// generates hundreds of range requests, so a per-request table would show
// the same film several hundred times and answer no useful question. The
// server aggregates; this renders that aggregate.
function initVideo(signal){
  loadVideo(signal);
  // Two seconds rather than the ten used elsewhere: bandwidth is the
  // point of this page, and a rate that updates every ten seconds reads
  // as stale while a video is visibly playing.
  startPagePoll('video', loadVideo, 2000);
}
function loadVideo(signal){
  return api('video', {signal:signal}).then(function(d){ S.video = d; renderVideo(); }).catch(function(){});
}

// fmtRate renders a byte rate. It is separate from fmtBytes because a
// rate needs the "/s" and reads better in bits for network figures — but
// bytes are kept here to stay consistent with every other size on the
// dashboard.
function fmtRate(bps){
  if(!bps) return '0 B/s';
  return fmtBytes(bps) + '/s';
}

function renderVideo(){
  var d = S.video || {};
  var files = d.files || [];

  var na = $('#vid-not-attached');
  if(na) na.style.display = (d.attached === false) ? 'block' : 'none';

  var tc = $('#vid-totals');
  if(tc){
    tc.innerHTML =
      '<div class="card"><div class="label">Bandwidth</div><div class="value">'+fmtRate(d.total_bytes_per_sec)+'</div><div class="delta">'+fmtNum(d.window_seconds,0)+'s window</div></div>'+
      '<div class="card"><div class="label">Streaming Now</div><div class="value" style="color:var(--primary)">'+fmtNum(d.active_files)+'</div><div class="delta">files</div></div>'+
      '<div class="card"><div class="label">Tracked Files</div><div class="value">'+fmtNum(files.length)+'</div></div>'+
      '<div class="card"><div class="label">Total Sent</div><div class="value">'+fmtBytes(d.total_bytes)+'</div></div>'+
      '<div class="card"><div class="label">Requests</div><div class="value">'+fmtNum(d.total_requests)+'</div></div>';
  }

  var cnt = $('#vid-count');
  if(cnt) cnt.textContent = files.length;

  var html = '';
  files.forEach(function(f){
    // A file with recent traffic is the one worth looking at, so it gets
    // the live badge; everything else is history that has not aged out.
    var badge = f.active
      ? '<span class="badge blue">streaming</span>'
      : '<span class="badge gray">idle</span>';

    // Disconnects are shown plainly rather than in red: abandoning a
    // response is how seeking works, and flagging it as a problem would
    // train the reader to ignore the column that does matter.
    var disc = f.disconnects ? fmtNum(f.disconnects) : '-';
    var errs = f.errors ? '<span class="badge red">'+fmtNum(f.errors)+'</span>' : '-';
    var cached = f.not_modified ? fmtNum(f.not_modified) : '-';

    var fileLabel = f.mount ? f.mount+' · '+f.file : f.file;
    html += '<tr>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:320px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(fileLabel)+'</td>'+
      '<td>'+badge+'</td>'+
      '<td style="font-family:var(--mono)">'+fmtRate(f.bytes_per_sec)+'</td>'+
      '<td>'+fmtBytes(f.bytes)+'</td>'+
      '<td>'+fmtNum(f.requests)+'</td>'+
      '<td>'+fmtNum(f.partial)+'</td>'+
      '<td>'+cached+'</td>'+
      '<td>'+disc+'</td>'+
      '<td>'+errs+'</td>'+
      '<td>'+fmtMS(f.avg_ms)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+fmtTime(f.last_seen)+'</td>'+
      '</tr>';
  });
  if(!files.length){
    html = '<tr><td colspan="11" class="empty">No video served yet'+
      (d.attached === false ? ' — attach video tracking first' : '')+'</td></tr>';
  }
  var tb = $('#vid-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── Database Browser ──────────────────────────────────────────────────
// The server side (table list, paged rows, search, and — with Config.AllowWrites and a DBWriter —
// insert, update and delete) existed, with a template, but the page was neither routed nor linked,
// and the script had no code for it.
function initDatabase(signal){
  S.dbTables = S.dbTables || [];
  loadDBTables(signal);
  startPagePoll('database', loadDBTables, 30000);
  if(S.dbTable) loadDBData(signal);
}
function loadDBTables(signal){
  return api('db/tables', {signal:signal}).then(function(d){ S.dbTables = (d && d.tables) || []; renderDBTables(); }).catch(function(){});
}
function renderDBTables(){
  var box = $('#db-tables');
  if(!box) return;
  var tables = S.dbTables || [];
  if(!box.querySelector('.db-side-head')){
    box.innerHTML = '<div class="db-side-head">Tables <span class="count-pill" id="db-count">0</span></div>'+
      '<input id="db-filter" type="search" placeholder="Filter tables" aria-label="Filter tables" value="'+attr(S._dbFilter||'')+'">'+
      '<div class="db-list" id="db-list"></div>';
    var fi = $('#db-filter');
    pageOn(fi, 'input', function(){ S._dbFilter = fi.value; renderDBTables(); });
  }
  var cnt = $('#db-count'); if(cnt) cnt.textContent = tables.length;
  var list = $('#db-list');
  var q = (S._dbFilter||'').toLowerCase();
  var shown = tables.filter(function(t){ return !q || t.name.toLowerCase().indexOf(q) >= 0; });
  if(!tables.length){
    list.innerHTML = '<div class="empty" style="padding:24px"><div class="icon">\u25a3</div>No database attached.<br><small>Call Collector.SetDBInspector to browse your tables here.</small></div>';
    return;
  }
  if(list.querySelector('.empty')) list.textContent = '';
  reconcileList(list, shown, function(t){return t.name;},
    function(t){return t.name+'|'+t.rows+'|'+(S.dbTable===t.name);},
    function(t){
      var node = el('div', {class:'db-table-item', role:'button', tabindex:'0'}, [
        el('span', {class:'nm', text:t.name}), el('span', {class:'ct', text:fmtNum(t.rows)})
      ]);
      var go = function(){ selectDBTable(t.name); };
      node.addEventListener('click', go);
      node.addEventListener('keydown', function(e){ if(e.key==='Enter' || e.key===' '){ e.preventDefault(); go(); } });
      node.classList.toggle('active', S.dbTable === t.name);
      return node;
    },
    function(node, t){
      node.querySelector('.ct').textContent = fmtNum(t.rows);
      node.classList.toggle('active', S.dbTable === t.name);
    });
  $$('#db-list .db-table-item').forEach(function(n){ n.classList.toggle('active', n.querySelector('.nm').textContent === S.dbTable); });
}
function selectDBTable(name){
  S.dbTable = name;
  S.dbState = {page:1, pageSize:S.dbState.pageSize||50, search:''};
  S.dbData = null;
  renderDBTables();
  loadDBData(currentPageSignal());
}
function loadDBData(signal){
  var st = S.dbState, name = S.dbTable;
  if(!name) return Promise.resolve();
  var path = 'db/tables/'+encodeURIComponent(name)+'?page='+st.page+'&page_size='+st.pageSize+(st.search?'&search='+encodeURIComponent(st.search):'');
  var box = $('#db-data');
  if(box && !S.dbData) box.innerHTML = '<div class="empty" style="padding:40px">Loading '+escapeHTML(name)+'\u2026</div>';
  return api(path, {signal:signal}).then(function(d){
    if(S.dbTable !== name) return;
    S.dbData = d;
    renderDBData();
  }).catch(function(e){
    if(e && e.name === 'AbortError') return;
    if(box && !S.dbData) box.innerHTML = '<div class="empty" style="padding:40px"><div class="icon">!</div>Could not read '+escapeHTML(name)+'.</div>';
  });
}
function dbCell(v){
  if(v === null || v === undefined) return '<span class="null">NULL</span>';
  var s = typeof v === 'object' ? JSON.stringify(v) : String(v);
  var more = s.length > 140;
  return '<span'+(more ? ' title="'+attr(s.slice(0, 600))+'"' : '')+'>'+escapeHTML(more ? s.slice(0, 140)+'\u2026' : s)+'</span>';
}
function dbPK(cols, row){
  return cols.filter(function(c){ return c.primary_key; }).map(function(c){
    return encodeURIComponent(c.name)+'='+encodeURIComponent(row[c.name]==null ? '' : row[c.name]);
  }).join(',');
}
function renderDBData(){
  var box = $('#db-data'), d = S.dbData;
  if(!box || !d) return;
  var cols = d.columns || [], rows = d.rows || [];
  var writable = !!d.writable && cols.some(function(c){ return c.primary_key; });
  var pages = Math.max(1, Math.ceil((d.total||0) / (d.page_size||S.dbState.pageSize)));
  var head = '<div class="db-toolbar"><h3>'+escapeHTML(d.table)+' <span class="count-pill">'+fmtNum(d.total)+'</span></h3>'+
    '<input id="db-search" type="search" placeholder="Search rows" aria-label="Search rows" value="'+attr(S.dbState.search)+'">'+
    '<select id="db-size" aria-label="Rows per page">'+[25,50,100,200].map(function(n){return '<option'+(n===S.dbState.pageSize?' selected':'')+'>'+n+'</option>';}).join('')+'</select>'+
    '<button id="db-refresh" type="button">Refresh</button>'+
    (writable ? '<button class="primary" id="db-new" type="button">+ New row</button>' : '')+'</div>';
  var th = cols.map(function(c){
    return '<th title="'+attr(c.type||'')+'">'+escapeHTML(c.name)+(c.primary_key?' <span class="pk">PK</span>':'')+'</th>';
  }).join('') + (writable ? '<th></th>' : '');
  var body = rows.map(function(r, i){
    return '<tr'+(S._dbFresh && S._dbFresh===i ? ' class="row-new"' : '')+'>'+cols.map(function(c){ return '<td>'+dbCell(r[c.name])+'</td>'; }).join('')+
      (writable ? '<td class="db-actions"><button class="db-edit" data-i="'+i+'" type="button">Edit</button> <button class="db-del" data-i="'+i+'" type="button">Delete</button></td>' : '')+'</tr>';
  }).join('');
  if(!rows.length) body = '<tr><td colspan="'+(cols.length+(writable?1:0)||1)+'" class="empty">'+(S.dbState.search ? 'No rows match "'+escapeHTML(S.dbState.search)+'"' : 'This table is empty')+'</td></tr>';
  box.innerHTML = head+'<div class="table-scroll"><table><thead><tr>'+th+'</tr></thead><tbody>'+body+'</tbody></table></div>'+
    '<div class="db-pager"><button id="db-prev" type="button"'+(S.dbState.page<=1?' disabled':'')+'>\u2039 Prev</button>'+
    '<span>Page '+S.dbState.page+' of '+pages+'</span>'+
    '<button id="db-next" type="button"'+(S.dbState.page>=pages?' disabled':'')+'>Next \u203a</button>'+
    (d.writable && !writable ? '<span class="db-note">Writes need a primary key; this table has none.</span>' : '')+'</div>';

  var go = function(){ loadDBData(currentPageSignal()); };
  var si = $('#db-search'), timer = 0;
  pageOn(si, 'input', function(){
    clearTimeout(timer);
    timer = setTimeout(function(){ S.dbState.search = si.value.trim(); S.dbState.page = 1; go(); }, 300);
  });
  pageOn($('#db-size'), 'change', function(){ S.dbState.pageSize = parseInt(this.value, 10) || 50; S.dbState.page = 1; go(); });
  pageOn($('#db-refresh'), 'click', go);
  pageOn($('#db-prev'), 'click', function(){ if(S.dbState.page > 1){ S.dbState.page--; go(); } });
  pageOn($('#db-next'), 'click', function(){ if(S.dbState.page < pages){ S.dbState.page++; go(); } });
  var nb = $('#db-new'); if(nb) pageOn(nb, 'click', function(){ openDBForm(null); });
  $$('.db-edit').forEach(function(b){ pageOn(b, 'click', function(){ openDBForm(rows[parseInt(b.dataset.i, 10)]); }); });
  $$('.db-del').forEach(function(b){
    pageOn(b, 'click', function(){
      var row = rows[parseInt(b.dataset.i, 10)];
      if(!window.confirm('Delete this row from '+d.table+'? This cannot be undone.')) return;
      b.disabled = true;
      fetch(S.base+'/api/db/tables/'+encodeURIComponent(d.table)+'/rows/'+dbPK(cols, row)+'', {method:'DELETE'}).then(function(r){
        if(r.status === 401){ window.location.href = loginURL(); return; }
        if(r.ok){ toast('Row deleted', 'ok'); loadDBData(currentPageSignal()); loadDBTables(currentPageSignal()); return; }
        return r.json().catch(function(){ return null; }).then(function(e){ b.disabled = false; toast('Delete failed: '+((e && e.error) || 'HTTP '+r.status), 'error', 6000); });
      }).catch(function(){ b.disabled = false; toast('Cannot reach the dashboard server', 'error'); });
    });
  });
}
// openDBForm shows the row editor: row is null for a new row.
function openDBForm(row){
  var d = S.dbData, cols = d.columns || [];
  var editing = !!row;
  closeDBForm();
  var fields = cols.map(function(c, i){
    var v = editing ? row[c.name] : '';
    var isNull = editing && v == null;
    var long = typeof v === 'string' && (v.length > 80 || v.indexOf('\n') >= 0);
    var input = long
      ? '<textarea data-col="'+attr(c.name)+'" rows="4"'+(editing && c.primary_key ? ' readonly' : '')+'>'+escapeHTML(isNull ? '' : v)+'</textarea>'
      : '<input data-col="'+attr(c.name)+'" value="'+attr(isNull || v == null ? '' : (typeof v === 'object' ? JSON.stringify(v) : v))+'"'+(editing && c.primary_key ? ' readonly' : '')+' spellcheck="false">';
    return '<div class="db-field"><label>'+escapeHTML(c.name)+' <small>'+escapeHTML(c.type||'')+(c.primary_key?' \u00b7 PK':'')+'</small></label>'+input+
      (c.nullable && !(editing && c.primary_key) ? '<label class="db-null"><input type="checkbox" class="chk" data-null="'+attr(c.name)+'"'+(isNull?' checked':'')+'> NULL</label>' : '')+'</div>';
  }).join('');
  var dlg = el('div', {class:'modal-scrim', id:'db-modal'}, []);
  dlg.innerHTML = '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="db-modal-title">'+
    '<h3 id="db-modal-title">'+(editing?'Edit row':'New row')+' \u00b7 '+escapeHTML(d.table)+'</h3>'+
    '<div class="db-fields">'+fields+'</div>'+
    '<div class="db-error" id="db-form-error" role="alert"></div>'+
    '<div class="modal-actions"><button type="button" id="db-cancel">Cancel</button><button type="button" class="primary" id="db-save">Save</button></div></div>';
  document.body.appendChild(dlg);
  requestAnimationFrame(function(){ dlg.classList.add('in'); });
  var first = dlg.querySelector('input:not([readonly]):not([type=checkbox]), textarea:not([readonly])');
  if(first) first.focus();
  var opener = document.activeElement;
  function close(){ closeDBForm(); }
  dlg.addEventListener('click', function(e){ if(e.target === dlg) close(); });
  dlg.addEventListener('keydown', function(e){ if(e.key === 'Escape'){ e.preventDefault(); close(); } });
  $('#db-cancel').addEventListener('click', close);
  $('#db-save').addEventListener('click', function(){
    var values = {};
    $$('#db-modal [data-col]').forEach(function(inp){
      var col = cols.filter(function(c){ return c.name === inp.dataset.col; })[0];
      if(editing && col.primary_key) return;
      var nullBox = $('#db-modal [data-null="'+inp.dataset.col.replace(/"/g, '\\"')+'"]');
      if(nullBox && nullBox.checked){ values[col.name] = null; return; }
      var s = inp.value;
      if(/^(bool|boolean)$/i.test(col.type||'')) values[col.name] = (s === 'true' || s === '1' || s === 't');
      else if(/int|serial|numeric|decimal|float|double|real|number/i.test(col.type||'') && s !== '' && isFinite(Number(s))) values[col.name] = Number(s);
      else values[col.name] = s;
    });
    var btn = $('#db-save'); btn.disabled = true; btn.textContent = 'Saving\u2026';
    var url = S.base+'/api/db/tables/'+encodeURIComponent(d.table)+'/rows'+(editing ? '/'+dbPK(cols, row) : '');
    fetch(url, {method: editing ? 'PUT' : 'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({values: values})}).then(function(r){
      if(r.status === 401){ window.location.href = loginURL(); return; }
      return r.json().catch(function(){ return null; }).then(function(res){
        if(r.ok){
          closeDBForm();
          toast(editing ? 'Row updated' : 'Row added', 'ok');
          loadDBData(currentPageSignal()); loadDBTables(currentPageSignal());
        } else {
          btn.disabled = false; btn.textContent = 'Save';
          $('#db-form-error').textContent = (res && res.error) || ('HTTP '+r.status);
        }
      });
    }).catch(function(){ btn.disabled = false; btn.textContent = 'Save'; $('#db-form-error').textContent = 'Cannot reach the dashboard server'; });
  });
  dlg._opener = opener;
}
function closeDBForm(){
  var m = document.getElementById('db-modal');
  if(!m) return;
  m.classList.remove('in');
  setTimeout(function(){ if(m.parentNode) m.parentNode.removeChild(m); }, motionOK() ? 200 : 0);
}

// ─── Mobile navigation ─────────────────────────────────────────────────
// Below 900px the sidebar is hidden, and nothing replaced it: the pages could only be reached by
// typing their addresses. The topbar's menu button opens it as a drawer.
function openNavDrawer(){
  document.body.classList.add('nav-open');
  var b = $('#nav-toggle'); if(b) b.setAttribute('aria-expanded', 'true');
  var first = $('#dashboard-nav .nav-item.active') || $('#dashboard-nav .nav-item');
  if(first) first.focus();
}
function navIsOpen(){
  return !!(document.body && document.body.classList && document.body.classList.contains('nav-open'));
}
function closeNavDrawer(){
  if(!navIsOpen()) return;
  document.body.classList.remove('nav-open');
  var b = $('#nav-toggle'); if(b){ b.setAttribute('aria-expanded', 'false'); }
}
function initNavDrawer(){
  var b = $('#nav-toggle');
  if(b) b.addEventListener('click', function(){
    if(navIsOpen()) closeNavDrawer(); else openNavDrawer();
  });
  var scrim = $('#nav-scrim');
  if(scrim) scrim.addEventListener('click', function(){ closeNavDrawer(); if(b) b.focus(); });
  document.addEventListener('keydown', function(e){
    if(e.key === 'Escape' && navIsOpen()){ closeNavDrawer(); if(b) b.focus(); }
  });
  var nav = $('#dashboard-nav');
  if(nav) nav.addEventListener('click', function(e){ if(e.target.closest && e.target.closest('.nav-item')) closeNavDrawer(); });
}

// ─── Theme ─────────────────────────────────────────────────────────────
// Canvas can't inherit CSS, so the chart palette is read back out of the
// stylesheet. That keeps every colour defined in dashboard.css.
function cssVar(name, fallback){
  try{
    var v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  }catch(e){ return fallback; }
}
var CHART_FALLBACK = ['#4f46e5','#12a37a','#8b5cf6','#d97706','#e11d48','#0891b2','#ea580c','#7c3aed'];
function chartColor(n){ return cssVar('--chart-'+n, CHART_FALLBACK[n-1]); }

// Resolved theme, honouring an explicit choice then the OS preference.
function currentTheme(){
  var t = document.documentElement.getAttribute('data-theme');
  if(t==='dark'||t==='light') return t;
  try{
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }catch(e){ return 'light'; }
}

function setTheme(t){
  var root = document.documentElement;
  // Surfaces cross-fade into the new palette instead of snapping (not for reduced motion). The
  // typeof guard keeps this function self-contained: the root-level poll harness evaluates it alone.
  if(typeof motionOK === 'function' && motionOK() && root.classList){
    root.classList.add('theme-fade');
    setTimeout(function(){ root.classList.remove('theme-fade'); }, 380);
  }
  root.setAttribute('data-theme', t);
  try{ localStorage.setItem('breeze-theme', t); }catch(e){}
  // Repaint from the data already in memory. A theme switch is purely
  // presentational; it must not trigger fresh API traffic or race a page
  // navigation just to redraw charts/canvases.
  try{
    if(S.page==='overview') renderOverview();
    else if(S.page==='performance') renderPerformance();
    else if(S.page==='cache') renderCache();
    else if(S.page==='fleet'){
      renderFleetCards(S.fleet.services, S.fleet.incidents);
      renderFleetTopology(S.fleet.topology, S.fleet.services, S.fleet.playback);
      renderFleetTraces(S.fleet.traces);
    }
  }catch(e){}
}

function initTheme(){
  var btn = document.getElementById('theme-toggle');
  if(!btn) return;
  // The theme toggle sits outside #breeze-app and belongs to the dashboard
  // shell for its entire lifetime, not to whichever page is currently active.
  btn.addEventListener('click', function(){
    setTheme(currentTheme()==='dark' ? 'light' : 'dark');
  });
}

// ─── Canvas charts ─────────────────────────────────────────────────────
// withAlpha returns the colour with an alpha, for the fade under the line. It understands the
// forms the stylesheet and a custom theme use (#rgb, #rrggbb, rgb(), rgba()); the old code
// appended hex digits to whatever it was given, and a colour in any other form threw from
// addColorStop and left the chart blank.
function withAlpha(color, a){
  var c = String(color||'').trim(), m;
  if((m = /^#([0-9a-f]{3})$/i.exec(c))) c = '#' + m[1].split('').map(function(x){return x+x;}).join('');
  if((m = /^#([0-9a-f]{6})$/i.exec(c))){
    var n = parseInt(m[1], 16);
    return 'rgba('+((n>>16)&255)+','+((n>>8)&255)+','+(n&255)+','+a+')';
  }
  if((m = /^rgba?\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)/i.exec(c))) return 'rgba('+m[1]+','+m[2]+','+m[3]+','+a+')';
  return null;
}
function roundRectPath(ctx, x, y, w, h, r){
  ctx.beginPath();
  ctx.moveTo(x+r, y); ctx.lineTo(x+w-r, y); ctx.quadraticCurveTo(x+w, y, x+w, y+r);
  ctx.lineTo(x+w, y+h-r); ctx.quadraticCurveTo(x+w, y+h, x+w-r, y+h);
  ctx.lineTo(x+r, y+h); ctx.quadraticCurveTo(x, y+h, x, y+h-r);
  ctx.lineTo(x, y+r); ctx.quadraticCurveTo(x, y, x+r, y); ctx.closePath();
}
// drawLineChart(canvas, data, color, opts): opts.times (ISO strings, one per point), opts.fmt
// (value -> text for the hover label) and opts.label (the accessible name). The first draw of a
// canvas sweeps the line in from the left; later draws are immediate, so a chart that updates
// every second does not keep re-animating.
function drawLineChart(canvas, data, color, opts){
  if(!canvas) return;
  opts = opts || {};
  var st = canvas._chart || (canvas._chart = {hover:null});
  st.data = data || [];
  st.color = color || chartColor(1);
  st.times = opts.times || null;
  st.fmt = opts.fmt || formatLabel;
  canvas.setAttribute('role', 'img');
  var last = st.data.length ? st.data[st.data.length-1] : null;
  canvas.setAttribute('aria-label', (opts.label || 'Chart') + (last==null ? ': no data' : ': latest ' + st.fmt(last)));
  if(!canvas._wired){
    canvas._wired = true;
    canvas.addEventListener('mousemove', function(e){
      var s = canvas._chart;
      if(!s || s.data.length < 2) return;
      var rect = canvas.getBoundingClientRect();
      var idx = Math.round(((e.clientX - rect.left - 40) / Math.max(1, rect.width - 50)) * (s.data.length - 1));
      idx = Math.max(0, Math.min(s.data.length - 1, idx));
      if(s.hover !== idx){ s.hover = idx; paintChart(canvas, 1); }
    });
    canvas.addEventListener('mouseleave', function(){
      var s = canvas._chart;
      if(s && s.hover != null){ s.hover = null; paintChart(canvas, 1); }
    });
  }
  if(!canvas._seen && st.data.length > 1 && motionOK()){
    canvas._seen = true;
    var t0 = null;
    var frame = function(ts){
      if(t0 === null) t0 = ts;
      var p = Math.min(1, (ts - t0) / 700);
      paintChart(canvas, 1 - Math.pow(1 - p, 3));
      if(p < 1 && canvas.isConnected) requestAnimationFrame(frame);
    };
    requestAnimationFrame(frame);
    return;
  }
  canvas._seen = true;
  paintChart(canvas, 1);
}
function paintChart(canvas, progress){
  var st = canvas._chart;
  if(!st) return;
  var data = st.data, lineColor = st.color;
  // Resolved per draw, so a theme switch repaints in the new palette.
  var gridColor = cssVar('--chart-grid', '#e4e5f7');
  var labelColor = cssVar('--chart-label', '#8b87ab');

  var dpr = window.devicePixelRatio || 1;

  var w = canvas.clientWidth || 600;
  var h = canvas.clientHeight || 200;
  if(canvas.width !== Math.round(w*dpr) || canvas.height !== Math.round(h*dpr)){
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
  }
  var ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  ctx.strokeStyle = gridColor;
  ctx.lineWidth = 1;

  ctx.beginPath();
  for(var i=0;i<=4;i++){
    var y = (i/4)*(h-20)+10;
    ctx.moveTo(40, y); ctx.lineTo(w-10, y);
  }
  ctx.stroke();
  if(!data.length){
    ctx.fillStyle = labelColor;
    ctx.font = '11px sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('No data', w/2, h/2);
    return;
  }
  var min = Math.min.apply(null, data);
  var max = Math.max.apply(null, data);
  if(min===max){min=min-1; max=max+1;}
  var pad = (max-min)*0.1;
  min -= pad; max += pad;
  ctx.fillStyle = labelColor;
  ctx.font = '10px monospace';
  ctx.textAlign = 'right';
  // The axis shows as many decimals as its range needs to keep neighbouring labels distinct: a
  // 6.0-7.0 axis used to print 7, 7, 7, 6, 6.
  var step = (max - min) / 4;
  for(var j=0;j<=4;j++){
    var v = max - (j/4)*(max-min);
    var ly = (j/4)*(h-20)+14;
    ctx.fillText(axisLabel(v, step), 36, ly);
  }
  var x0 = 40, w0 = w - 50;
  function px(k){ return x0 + (k/Math.max(1,data.length-1))*w0; }
  function py(val){ return (h-20) - ((val-min)/(max-min))*(h-30) + 10; }

  ctx.save();
  if(progress < 1){
    ctx.beginPath(); ctx.rect(0, 0, x0 + w0 * progress + 2, h); ctx.clip();
  }
  ctx.strokeStyle = lineColor;
  ctx.lineWidth = 2;
  ctx.lineJoin = 'round';
  ctx.beginPath();
  for(var k=0;k<data.length;k++){
    if(k===0) ctx.moveTo(px(k), py(data[k])); else ctx.lineTo(px(k), py(data[k]));
  }
  ctx.stroke();
  ctx.lineTo(x0 + w0, h-10);
  ctx.lineTo(x0, h-10);
  ctx.closePath();
  // Vertical fade under the line reads as depth without hiding the grid.
  var top = withAlpha(lineColor, 0.24), bottom = withAlpha(lineColor, 0.02);
  if(top){
    var grad = ctx.createLinearGradient(0, 10, 0, h-10);
    grad.addColorStop(0, top);
    grad.addColorStop(1, bottom);
    ctx.fillStyle = grad;
  } else {
    ctx.globalAlpha = 0.12; ctx.fillStyle = lineColor;
  }
  ctx.fill();
  ctx.restore();

  // Hover: a guide, the point, and its value (and time).
  if(st.hover != null && progress >= 1 && st.hover < data.length){
    var hx = px(st.hover), hy = py(data[st.hover]);
    ctx.strokeStyle = labelColor; ctx.lineWidth = 1; ctx.setLineDash([3, 3]);
    ctx.beginPath(); ctx.moveTo(hx, 10); ctx.lineTo(hx, h-10); ctx.stroke(); ctx.setLineDash([]);
    ctx.fillStyle = lineColor;
    ctx.beginPath(); ctx.arc(hx, hy, 4, 0, Math.PI*2); ctx.fill();
    ctx.strokeStyle = cssVar('--surface', '#fff'); ctx.lineWidth = 2; ctx.stroke();
    var text = st.fmt(data[st.hover]);
    var when = st.times && st.times[st.hover] ? fmtTime(st.times[st.hover]) : '';
    ctx.font = '600 11px sans-serif';
    var tw = Math.max(ctx.measureText(text).width, when ? ctx.measureText(when).width : 0) + 16;
    var th = when ? 34 : 22;
    var bx = hx + 10; if(bx + tw > w - 4) bx = hx - 10 - tw;
    var by = Math.max(4, Math.min(h - th - 4, hy - th/2));
    roundRectPath(ctx, bx, by, tw, th, 6);
    ctx.fillStyle = cssVar('--surface', '#fff'); ctx.fill();
    ctx.strokeStyle = gridColor; ctx.lineWidth = 1; ctx.stroke();
    ctx.fillStyle = cssVar('--text', '#222'); ctx.textAlign = 'left';
    ctx.fillText(text, bx + 8, by + 15);
    if(when){ ctx.font = '10px sans-serif'; ctx.fillStyle = labelColor; ctx.fillText(when, bx + 8, by + 28); }
  }
}
function axisLabel(v, step){
  if(Math.abs(v) >= 1000) return formatLabel(v);
  var decimals = step >= 1 ? 0 : (step >= 0.1 ? 1 : (step >= 0.01 ? 2 : 3));
  return v.toFixed(decimals);
}
function formatLabel(v){
  if(Math.abs(v) >= 1000000) return (v/1000000).toFixed(1)+'M';
  if(Math.abs(v) >= 1000) return (v/1000).toFixed(1)+'k';
  if(Math.abs(v) < 1) return v.toFixed(2);
  return Math.round(v).toString();
}

// ─── Init ──────────────────────────────────────────────────────────────
function loadCaps(){
  api('capabilities').then(function(c){ S.caps = Object.assign(S.caps, c || {}); }).catch(function(){});
}
function init(){
  // The server writes its base path into the page. Guessing it from the address (the pattern was
  // /^(\/[^\/]*dashboard)/) was wrong for every base_path that does not end in "dashboard": the
  // API calls and the WebSocket then went to /dashboard and the page never loaded any data.
  var declared = document.body && document.body.getAttribute('data-base-path');
  if(declared){
    S.base = declared.replace(/\/+$/, '');
  } else {
    var match = location.pathname.match(/^(\/[^\/]*dashboard)/);
    S.base = match ? match[1] : '/dashboard';
  }
  initTheme();
  initNavDrawer();
  connectWS();
  loadCaps();


  // Fix: if a page script already set __breezeDashPage before dashboard.js
  // loaded (happens on initial page load, not SPA navigation), init that
  // page now. This fixes the "blank page after relogin" bug.
  if(window.__breezeDashPage){
    initPage(window.__breezeDashPage);
    window.__breezeDashPage = null;
  }
}
init();
window.BreezeDash = {initPage: initPage, api: api, apiPost: apiPost, S: S};
// The line above is the anchor the fleet playback harness (testdata/fleet_playback_harness.js)
// appends its test seam to, so it is left exactly as it was; additions go on their own line.
window.BreezeDash.toast = toast;
})();
