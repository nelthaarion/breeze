package dashboard

// spaJS contains all the dashboard's JavaScript, inlined into the SPA shell.
//
// The runtime is a tiny SPA framework with:
//   - hash-based routing (#/overview, #/routes, ...)
//   - a single WebSocket connection for live updates
//   - lightweight virtual-scrolling table renderer for large lists
//   - canvas-based mini-charts (no external dependencies)
//
// All API calls go through the dashboard's own JSON endpoints under
// /dashboard/api/.
const spaJS = `
(function(){
'use strict';

// ─── State ─────────────────────────────────────────────────────────────
var S = {
  base: location.pathname.replace(/\/$/, ''),
  ws: null,
  wsConnected: false,
  snapshot: null,
  history: [],
  page: 'overview',
  // Per-page state
  requests: [],
  queries: [],
  timelines: [],
  logs: {app:[], http:[], error:[], panic:[], warning:[]},
  logTab: 'app',
  routes: [],
  apiRoutes: [],
  apiRouteSel: null,
  apiResp: null,
  apiSnippetLang: 'curl',
  dbTables: [],
  dbTableSel: null,
  dbData: null,
  dbPage: 1,
  filters: {method:'', status:'', route:'', user:''},
  reqFilter: {method:'', status:'', route:'', user:''},
  qSearch: '',
  logSearch: '',
  expandedRows: {},
  expandedSteps: {},
  expandedTimeline: null,
  _scroll: {}, // per-page scrollTop, keyed by page id
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
function fmtTime(t){
  if(!t) return '-';
  var d = new Date(t);
  if(isNaN(d)) return t;
  return d.toLocaleTimeString([], {hour12:false}) + '.' + String(d.getMilliseconds()).padStart(3,'0');
}
function fmtDate(t){
  if(!t) return '-';
  var d = new Date(t);
  if(isNaN(d)) return t;
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
function api(path, opts){
  opts = opts||{};
  return fetch(S.base + '/api/' + path, opts).then(function(r){
    if(r.status === 401){
      window.location.href = S.base + '/login';
      throw new Error('unauthorized');
    }
    if(!r.ok) throw new Error('HTTP '+r.status);
    return r.json();
  });
}
function apiPost(path, body){
  return fetch(S.base + '/api/' + path, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body||{})
  }).then(function(r){
    if(r.status === 401){
      window.location.href = S.base + '/login';
      throw new Error('unauthorized');
    }
    return r.json();
  });
}
function apiSend(path, method, body){
  return fetch(S.base + '/api/' + path, {
    method: method,
    headers: {'Content-Type': 'application/json'},
    body: body===undefined ? undefined : JSON.stringify(body)
  }).then(function(r){
    if(r.status === 401){
      window.location.href = S.base + '/login';
      throw new Error('unauthorized');
    }
    return r.json().catch(function(){return {};}).then(function(data){
      return {ok:r.ok, status:r.status, data:data};
    });
  });
}

// ─── WebSocket ─────────────────────────────────────────────────────────
function connectWS(){
  var proto = location.protocol==='https:'?'wss:':'ws:';
  var url = proto + '//' + location.host + S.base + '/ws';
  try {
    S.ws = new WebSocket(url);
  } catch(e){
    setTimeout(connectWS, 3000);
    return;
  }
  S.ws.onopen = function(){
    S.wsConnected = true;
    updateWSIndicator();
  };
  S.ws.onclose = function(){
    S.wsConnected = false;
    updateWSIndicator();
    setTimeout(connectWS, 2000);
  };
  S.ws.onerror = function(){try{S.ws.close();}catch(e){}};
  S.ws.onmessage = function(ev){
    var msg;
    try { msg = JSON.parse(ev.data); } catch(e){ return; }
    if(msg.type==='snapshot'){
      S.snapshot = msg;
      if(S.history.length>120) S.history.shift();
      S.history.push(msg.metrics);
      onSnapshot();
    } else if(msg.type==='event'){
      onEvent(msg.channel, msg.data);
    }
  };
}
function updateWSIndicator(){
  var dot = $('.ws-dot');
  if(dot) dot.className = 'ws-dot ' + (S.wsConnected?'on':'off');
  var txt = $('.ws-status');
  if(txt) txt.textContent = S.wsConnected?'live':'reconnecting...';
}

// ─── Snapshot dispatch ─────────────────────────────────────────────────
function onSnapshot(){
  if(S.page==='overview') renderOverview();
}
function onEvent(ch, data){
  if(ch==='request'){
    S.requests.push(data);
    if(S.requests.length>500) S.requests.shift();
    if(S.page==='requests') renderRequests();
    if(S.page==='overview') {/* metrics will refresh on next snapshot */}
  } else if(ch==='query'){
    S.queries.push(data);
    if(S.queries.length>300) S.queries.shift();
    if(S.page==='queries') renderQueries();
  } else if(ch==='timeline'){
    S.timelines.unshift(data);
    if(S.timelines.length>50) S.timelines.pop();
    if(S.page==='timeline') renderTimelineList();
  }
}

// ─── Routing ───────────────────────────────────────────────────────────
var PAGES = [
  ['overview', 'Overview', 'M 3 12 L 12 3 L 21 12 M 5 10 L 5 20 L 19 20 L 19 10'],
  ['routes', 'Routes', 'M 4 6 L 20 6 M 4 12 L 20 12 M 4 18 L 14 18'],
  ['api', 'API Explorer', 'M 4 4 L 4 20 L 20 20 L 20 4 Z M 8 8 L 16 8 M 8 12 L 16 12 M 8 16 L 13 16'],
  ['requests', 'Live Requests', 'M 4 4 L 4 16 L 8 16 L 8 20 L 12 16 L 20 16 L 20 4 Z'],
  ['database', 'Database', 'M 4 5 C 4 3.9 7.6 3 12 3 C 16.4 3 20 3.9 20 5 L 20 19 C 20 20.1 16.4 21 12 21 C 7.6 21 4 20.1 4 19 Z M 4 5 C 4 6.1 7.6 7 12 7 C 16.4 7 20 6.1 20 5'],
  ['queries', 'ORM Queries', 'M 5 5 L 19 5 L 19 19 L 5 19 Z M 8 9 L 16 9 M 8 13 L 13 13'],
  ['cache', 'Cache', 'M 4 7 L 12 3 L 20 7 L 20 17 L 12 21 L 4 17 Z'],
  ['queue', 'Queue', 'M 4 8 L 20 8 L 20 16 L 4 16 Z M 6 11 L 10 11 M 6 13 L 9 13'],
  ['scheduler', 'Scheduler', 'M 12 7 L 12 12 L 16 14 M 12 21 C 7 21 3 17 3 12 C 3 7 7 3 12 3 C 17 3 21 7 21 12 C 21 17 17 21 12 21 Z'],
  ['logs', 'Logs', 'M 4 4 L 4 20 L 20 20 M 8 8 L 16 8 M 8 12 L 16 12 M 8 16 L 13 16'],
  ['health', 'Health', 'M 12 21 C 7 21 3 17 3 12 C 3 7 7 3 12 3 C 17 3 21 7 21 12 C 21 17 17 21 12 21 Z M 9 12 L 11 14 L 15 10'],
  ['performance', 'Performance', 'M 3 17 L 9 11 L 13 15 L 21 7 M 14 7 L 21 7 L 21 14'],
  ['timeline', 'Timeline', 'M 4 4 L 4 20 M 8 6 L 16 6 M 8 10 L 14 10 M 8 14 L 18 14 M 8 18 L 12 18'],
];
function navHTML(){
  var sections = [
    {title:'Monitor', items:['overview','requests','timeline','performance']},
    {title:'Develop', items:['routes','api','database','queries']},
    {title:'System', items:['cache','queue','scheduler','logs','health']},
  ];
  var out = '';
  sections.forEach(function(sec){
    out += '<div class="nav-section">'+sec.title+'</div>';
    sec.items.forEach(function(id){
      var label = pageLabel(id);
      out += '<div class="nav-item" data-page="'+id+'">'+
        '<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">'+
        '<path d="'+pageIcon(id)+'"/></svg>'+
        '<span>'+label+'</span></div>';
    });
  });
  return out;
}
function pageLabel(id){for(var i=0;i<PAGES.length;i++) if(PAGES[i][0]===id) return PAGES[i][1]; return id;}
function pageIcon(id){for(var i=0;i<PAGES.length;i++) if(PAGES[i][0]===id) return PAGES[i][2]; return '';}

function validPage(id){
  return PAGES.some(function(p){return p[0]===id;});
}
function saveScroll(){
  var c = $('.content');
  if(c) S._scroll[S.page] = c.scrollTop;
}

// render() performs the actual DOM switch for a page. It never touches
// browser history — callers decide whether/how the URL should change.
function render(page){
  S.page = page;
  $$('.nav-item').forEach(function(n){
    n.classList.toggle('active', n.dataset.page===page);
  });
  $$('.page').forEach(function(p){p.classList.remove('active');});
  var p = $('#page-'+page);
  if(p) p.classList.add('active');
  $('.topbar h2').textContent = pageLabel(page);
  loadPage(page);
  var c = $('.content');
  if(c) c.scrollTop = S._scroll[page] || 0;
}

// go() is the entry point for app-initiated navigation (sidebar clicks, etc).
// It pushes a new history entry so Back/Forward have something to work with.
function go(page){
  if(!validPage(page)) page = 'overview';
  if(page === S.page && location.hash === '#/'+page) return; // no-op: avoids duplicate history entries
  saveScroll();
  render(page);
  if(history.pushState) history.pushState(null, '', '#/'+page);
}

// syncFromHash() re-syncs the UI with location.hash. Used for Back/Forward
// (popstate) and manual hash edits (hashchange) — it never itself calls
// pushState/replaceState, so it can't create loops or duplicate entries.
function syncFromHash(){
  var hash = location.hash.replace(/^#\//,'');
  var page = validPage(hash) ? hash : 'overview';
  if(page === S.page) return;
  saveScroll();
  render(page);
}
window.addEventListener('popstate', syncFromHash);
window.addEventListener('hashchange', syncFromHash);

function loadPage(page){
  switch(page){
    case 'overview': renderOverview(); break;
    case 'routes': if(S.routes.length) renderRoutes(); api('routes').then(function(d){S.routes=d;renderRoutes();}); break;
    case 'api': if(!S.apiRoutes.length) api('api-explorer').then(function(d){S.apiRoutes=d;renderAPIExplorer();}); else renderAPIExplorer(); break;
    case 'requests': renderRequests(); break;
    case 'database': renderDatabase(); api('db/tables').then(function(d){S.dbTables=d.tables||[];renderDatabase();}).catch(function(){}); break;
    case 'queries': renderQueries(); break;
    case 'cache': if(S.cache) renderCache(); api('cache').then(function(d){S.cache=d;renderCache();}); break;
    case 'queue': if(S.queue) renderQueue(); api('queue').then(function(d){S.queue=d;renderQueue();}); break;
    case 'scheduler': if(S.tasks) renderScheduler(); api('scheduler').then(function(d){S.tasks=d;renderScheduler();}); break;
    case 'logs': renderLogs(); break;
    case 'health': if(S.health) renderHealth(); api('health').then(function(d){S.health=d;renderHealth();}); break;
    case 'performance': if(S.perf) renderPerformance(); api('performance').then(function(d){S.perf=d;renderPerformance();}); break;
    case 'timeline': if(!S.timelines.length) api('timeline').then(function(d){S.timelines=d;renderTimelineList();}); else renderTimelineList(); break;
  }
}

// ─── Renderers ─────────────────────────────────────────────────────────
// Each render* function rebuilds the page's DOM into its container.

function renderOverview(){
  var m = S.snapshot ? S.snapshot.metrics : (S.history[S.history.length-1]||{});
  var cards = [
    {label:'Requests Today', value:fmtNum(m.requests_today), icon:'\u2191'},
    {label:'Requests / sec', value:fmtNum(m.requests_per_sec, 1), icon:'\u26a1'},
    {label:'Avg Response', value:fmtMS(m.avg_resp_time_ms), icon:'\u23f1'},
    {label:'Error Rate', value:fmtNum(m.error_rate*100, 2)+'%', icon:'\u26a0', cls:m.error_rate>0.05?'red':''},
    {label:'Active Sessions', value:fmtNum(m.active_sessions), icon:'\u25cf'},
    {label:'DB Connections', value:fmtNum(m.db_connections), icon:'\u25a3'},
    {label:'Cache Hit', value:fmtNum(m.cache_hit_ratio*100, 1)+'%', icon:'\u2713'},
    {label:'Queue Jobs', value:fmtNum(m.queue_jobs), icon:'\u2630'},
    {label:'Goroutines', value:fmtNum(m.goroutines), icon:'\u2261'},
    {label:'Heap Alloc', value:fmtBytes(m.heap_alloc), icon:'\u25c8'},
    {label:'Mem Sys', value:fmtBytes(m.mem_sys), icon:'\u25c9'},
    {label:'CPU Usage', value:fmtNum(m.cpu_usage, 1)+'%', icon:'\u25b2'},
  ];
  var html = '<div class="cards">';
  cards.forEach(function(c){
    html += '<div class="card">'+
      '<div class="label">'+c.label+'</div>'+
      '<div class="value '+(c.cls||'')+'">'+c.value+'</div>'+
      '<div class="delta">live</div>'+
      '<canvas class="spark" data-spark="'+c.label+'"></canvas>'+
      '</div>';
  });
  html += '</div>';

  html += '<div class="chart-row">'+
    '<div class="chart-card"><div class="head"><h3>Requests / second</h3></div><canvas id="chart-rps"></canvas></div>'+
    '<div class="chart-card"><div class="head"><h3>Response time (ms)</h3></div><canvas id="chart-latency"></canvas></div>'+
    '</div>';
  html += '<div class="chart-row">'+
    '<div class="chart-card"><div class="head"><h3>Memory (MB)</h3></div><canvas id="chart-mem"></canvas></div>'+
    '<div class="chart-card"><div class="head"><h3>Goroutines</h3></div><canvas id="chart-goroutines"></canvas></div>'+
    '</div>';

  var c = $('#page-overview');
  c.innerHTML = html;

  // Draw charts
  drawLineChart($('#chart-rps'), S.history.map(function(h){return h.requests_per_sec||0;}), '#58a6ff');
  drawLineChart($('#chart-latency'), S.history.map(function(h){return h.avg_resp_time_ms||0;}), '#3fb950');
  drawLineChart($('#chart-mem'), S.history.map(function(h){return (h.heap_alloc||0)/1024/1024;}), '#bc8cff');
  drawLineChart($('#chart-goroutines'), S.history.map(function(h){return h.goroutines||0;}), '#d29922');
}

function renderRoutes(){
  var routes = S.routes || [];
  var search = (S._routeSearch||'').toLowerCase();
  var filtered = routes.filter(function(r){
    if(!search) return true;
    return (r.pattern||'').toLowerCase().indexOf(search)>=0 ||
           (r.method||'').toLowerCase().indexOf(search)>=0;
  });
  var html = '<div class="table-wrap"><div class="table-head">'+
    '<h3>Routes ('+filtered.length+')</h3>'+
    '<div class="filters"><input id="route-search" placeholder="Search..." value="'+escapeHTML(S._routeSearch||'')+'"></div>'+
    '</div><div class="table-scroll"><table><thead><tr>'+
    '<th>Method</th><th>Path</th><th>Requests</th><th>Avg Latency</th><th>Max</th><th>Last</th><th>Errors</th>'+
    '</tr></thead><tbody>';
  filtered.forEach(function(r, i){
    html += '<tr class="clickable" data-idx="'+i+'">'+
      '<td><span class="method-pill '+r.method+'">'+r.method+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(r.pattern)+'</td>'+
      '<td>'+fmtNum(r.requests)+'</td>'+
      '<td>'+fmtMS(r.avg_latency_ms)+'</td>'+
      '<td>'+fmtMS(r.max_latency_ms)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(r.last_request?fmtDate(r.last_request):'-')+'</td>'+
      '<td>'+(r.errors>0?'<span class="badge red">'+r.errors+'</span>':'-')+'</td>'+
      '</tr>';
  });
  if(!filtered.length) html += '<tr><td colspan="7" class="empty">No routes registered</td></tr>';
  html += '</tbody></table></div></div>';
  $('#page-routes').innerHTML = html;
  var inp = $('#route-search');
  if(inp) inp.addEventListener('input', function(){S._routeSearch = inp.value; renderRoutes(); inp.focus();});
}

function renderAPIExplorer(){
  var c = $('#page-api');
  var sel = S.apiRouteSel;
  var html = '<div class="api-grid">'+
    '<div class="api-list"><div style="padding:10px 12px;border-bottom:1px solid var(--border);font-size:11px;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim);font-weight:600">Endpoints ('+S.apiRoutes.length+')</div>';
  S.apiRoutes.forEach(function(r, i){
    html += '<div class="item '+(sel===i?'active':'')+'" data-idx="'+i+'">'+
      '<span class="method-pill '+r.method+'">'+r.method+'</span>'+
      '<span style="font-family:var(--mono);font-size:11px">'+escapeHTML(r.path)+'</span>'+
      '</div>';
  });
  html += '</div><div class="api-detail">';
  if(sel!=null && S.apiRoutes[sel]){
    var r = S.apiRoutes[sel];
    html += '<div class="api-form">'+
      '<div class="row"><label>Method</label><div class="controls"><select id="api-method">'+
      ['GET','POST','PUT','PATCH','DELETE','OPTIONS'].map(function(m){return '<option '+(m===r.method?'selected':'')+'>'+m+'</option>';}).join('')+
      '</select></div></div>'+
      '<div class="row"><label>URL</label><div class="controls"><input id="api-url" value="'+escapeHTML(r.path)+'" placeholder="/path"></div></div>'+
      '<div class="row"><label>Headers</label><div><div class="headers" id="api-headers">'+
      '<input class="hk" placeholder="Key"><input class="hv" placeholder="Value"><button class="rm">-</button>'+
      '<input class="hk" placeholder="Key"><input class="hv" placeholder="Value"><button class="rm">-</button>'+
      '</div><button id="api-add-header" style="margin-top:6px">+ Header</button></div></div>'+
      '<div class="row"><label>Body</label><div><textarea id="api-body" rows="5" placeholder="{}"></textarea></div></div>'+
      '<div class="row"><label></label><div><button class="primary" id="api-send">Send</button></div></div>'+
      '</div>';
    if(S.apiResp){
      var r2 = S.apiResp;
      html += '<div class="api-response"><div class="head"><div><span class="status-pill '+statusClass(r2.status)+'">'+r2.status+'</span> '+
        '<span style="margin-left:8px;font-size:11px;color:var(--text-dim)">'+fmtMS(r2.duration_ms)+' \u00b7 '+fmtBytes(r2.size)+'</span></div>'+
        '<button id="api-clear">Clear</button></div>'+
        '<div class="body"><pre>'+escapeHTML(r2.body_json?JSON.stringify(r2.body_json, null, 2):r2.body)+'</pre></div></div>';
      html += '<div class="snippets"><div class="tabs">'+
        ['curl','go','javascript','python','csharp','php'].map(function(l){
          return '<div class="tab '+(S.apiSnippetLang===l?'active':'')+'" data-lang="'+l+'">'+l+'</div>';
        }).join('')+
        '</div><div class="tab-content"><button class="copy-btn">Copy</button><pre id="snippet-'+S.apiSnippetLang+'">'+
        escapeHTML((r2.snippets||{})[S.apiSnippetLang]||'')+
        '</pre></div></div>';
    }
  } else {
    html += '<div class="empty"><div class="icon">\u2197</div>Select an endpoint to begin</div>';
  }
  html += '</div></div>';
  c.innerHTML = html;

  // Wire up
  $$('.api-list .item').forEach(function(it){
    it.addEventListener('click', function(){
      S.apiRouteSel = parseInt(it.dataset.idx);
      S.apiResp = null;
      renderAPIExplorer();
    });
  });
  var addHdr = $('#api-add-header');
  if(addHdr) addHdr.addEventListener('click', function(){
    var c2 = $('#api-headers');
    c2.appendChild(el('input', {class:'hk', placeholder:'Key'}));
    c2.appendChild(el('input', {class:'hv', placeholder:'Value'}));
    var btn = el('button', {class:'rm', text:'-'});
    btn.addEventListener('click', function(){c2.removeChild(btn.previousSibling); c2.removeChild(btn.previousSibling); c2.removeChild(btn);});
    c2.appendChild(btn);
  });
  $$('.headers .rm').forEach(function(b){
    b.addEventListener('click', function(){b.parentNode.removeChild(b.previousSibling); b.parentNode.removeChild(b.previousSibling); b.parentNode.removeChild(b);});
  });
  var sendBtn = $('#api-send');
  if(sendBtn) sendBtn.addEventListener('click', function(){
    var headers = {};
    var ks = $$('.api-headers .hk');
    var vs = $$('.api-headers .hv');
    for(var i=0;i<ks.length;i++){
      if(ks[i].value) headers[ks[i].value] = vs[i].value;
    }
    var body = {
      method: $('#api-method').value,
      url: $('#api-url').value,
      headers: headers,
      body: $('#api-body').value
    };
    sendBtn.textContent = 'Sending...';
    apiPost('api-explorer', body).then(function(r){
      S.apiResp = r;
      renderAPIExplorer();
    }).catch(function(e){
      sendBtn.textContent = 'Send';
      alert('Error: '+e.message);
    });
  });
  var clearBtn = $('#api-clear');
  if(clearBtn) clearBtn.addEventListener('click', function(){S.apiResp=null; renderAPIExplorer();});
  $$('.snippets .tab').forEach(function(t){
    t.addEventListener('click', function(){
      S.apiSnippetLang = t.dataset.lang;
      renderAPIExplorer();
    });
  });
  var copyBtn = $('.copy-btn');
  if(copyBtn) copyBtn.addEventListener('click', function(){
    var txt = $('.snippets pre').textContent;
    navigator.clipboard.writeText(txt).then(function(){copyBtn.textContent='Copied!'; setTimeout(function(){copyBtn.textContent='Copy';},1500);});
  });
}

function renderRequests(){
  var list = S.requests.slice().reverse();
  // apply filters
  var f = S.reqFilter;
  if(f.method) list = list.filter(function(r){return r.method===f.method;});
  if(f.status) list = list.filter(function(r){return (''+r.status).startsWith(f.status.replace('xx',''));});
  if(f.route) list = list.filter(function(r){return (r.route||r.path||'').indexOf(f.route)>=0;});
  if(f.user) list = list.filter(function(r){return (r.user||'').toLowerCase().indexOf(f.user.toLowerCase())>=0;});

  var html = '<div class="table-wrap"><div class="table-head">'+
    '<h3>Live Requests ('+list.length+')</h3>'+
    '<div class="filters">'+
    '<select id="f-method"><option value="">All Methods</option>'+
    ['GET','POST','PUT','PATCH','DELETE','OPTIONS'].map(function(m){return '<option '+(f.method===m?'selected':'')+'>'+m+'</option>';}).join('')+
    '</select>'+
    '<select id="f-status"><option value="">All Status</option>'+
    ['2xx','3xx','4xx','5xx'].map(function(s){return '<option '+(f.status===s?'selected':'')+'>'+s+'</option>';}).join('')+
    '</select>'+
    '<input id="f-route" placeholder="Route filter" value="'+escapeHTML(f.route)+'">'+
    '<input id="f-user" placeholder="User filter" value="'+escapeHTML(f.user)+'">'+
    '</div></div><div class="table-scroll" id="req-scroll"><table><thead><tr>'+
    '<th>Time</th><th>Method</th><th>Path</th><th>Status</th><th>Duration</th><th>IP</th><th>User</th><th>Size</th><th>Timeline</th>'+
    '</tr></thead><tbody id="req-tbody">';
  // Virtual scrolling: show first 200 rows
  var max = 200;
  list.slice(0, max).forEach(function(r, i){
    var durSlow = r.duration_ms > 500;
    html += '<tr class="clickable" data-idx="'+i+'">'+
      '<td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(r.time)+'</td>'+
      '<td><span class="method-pill '+r.method+'">'+r.method+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:340px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(r.path)+'</td>'+
      '<td><span class="status-pill '+statusClass(r.status)+'">'+(r.status||'-')+'</span></td>'+
      '<td>'+(durSlow?'<span style="color:var(--err)">'+fmtMS(r.duration_ms)+'</span>':fmtMS(r.duration_ms))+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+escapeHTML(r.ip)+'</td>'+
      '<td style="font-size:11px">'+escapeHTML(r.user||'-')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+fmtBytes(r.resp_size)+'</td>'+
      '<td>'+(r.timeline_id?'<a href="#/timeline" data-tl="'+r.timeline_id+'">view</a>':'-')+'</td>'+
      '</tr>';
  });
  if(list.length>max) html += '<tr><td colspan="9" style="text-align:center;color:var(--text-muted);padding:8px">Showing latest '+max+' of '+list.length+' requests</td></tr>';
  if(!list.length) html += '<tr><td colspan="9" class="empty">No requests yet</td></tr>';
  html += '</tbody></table></div></div>';
  $('#page-requests').innerHTML = html;

  var fm = $('#f-method'); if(fm) fm.addEventListener('change', function(){S.reqFilter.method=fm.value; renderRequests();});
  var fs = $('#f-status'); if(fs) fs.addEventListener('change', function(){S.reqFilter.status=fs.value; renderRequests();});
  var fr = $('#f-route'); if(fr) fr.addEventListener('input', function(){S.reqFilter.route=fr.value; renderRequests(); fr.focus();});
  var fu = $('#f-user'); if(fu) fu.addEventListener('input', function(){S.reqFilter.user=fu.value; renderRequests(); fu.focus();});
  $$('a[data-tl]').forEach(function(a){
    a.addEventListener('click', function(e){
      e.preventDefault();
      S.expandedTimeline = a.dataset.tl;
      go('timeline');
    });
  });
}

function renderQueries(){
  var list = S.queries.slice().reverse();
  var slowOnly = S._slowOnly;
  if(slowOnly) list = list.filter(function(q){return q.slow;});
  var q = S.qSearch.toLowerCase();
  if(q) list = list.filter(function(x){return (x.sql||'').toLowerCase().indexOf(q)>=0;});

  var html = '<div class="table-wrap"><div class="table-head">'+
    '<h3>ORM Queries ('+list.length+')</h3>'+
    '<div class="filters">'+
    '<label style="font-size:11px"><input type="checkbox" id="slow-only" '+(slowOnly?'checked':'')+'> Slow only</label>'+
    '<input id="q-search" placeholder="Search SQL..." value="'+escapeHTML(S.qSearch)+'">'+
    '</div></div><div class="table-scroll"><table><thead><tr>'+
    '<th>Time</th><th>SQL</th><th>Duration</th><th>Rows</th><th>Location</th><th>Status</th>'+
    '</tr></thead><tbody>';
  list.slice(0, 300).forEach(function(q, i){
    var open = S.expandedSteps['q'+i];
    html += '<tr class="clickable" data-idx="'+i+'">'+
      '<td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(q.time)+'</td>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:540px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">'+escapeHTML(q.sql)+'</td>'+
      '<td>'+(q.slow?'<span class="badge red">slow</span> ':'')+fmtDur(q.duration_us)+'</td>'+
      '<td style="font-family:var(--mono)">'+fmtNum(q.rows)+'</td>'+
      '<td style="font-family:var(--mono);font-size:10px;color:var(--text-dim)">'+escapeHTML(q.file)+':'+q.line+'</td>'+
      '<td>'+(q.error?'<span class="badge red">error</span>':'<span class="badge green">ok</span>')+'</td>'+
      '</tr>';
    if(open){
      html += '<tr class="row-detail"><td colspan="6"><div class="inner">'+
        '<div style="margin-bottom:8px"><strong>SQL:</strong></div><pre>'+escapeHTML(q.sql)+'</pre>'+
        (q.args&&q.args.length?'<div style="margin:8px 0"><strong>Args:</strong> '+escapeHTML(JSON.stringify(q.args))+'</div>':'')+
        (q.error?'<div style="margin:8px 0"><strong>Error:</strong> <span style="color:var(--err)">'+escapeHTML(q.error)+'</span></div>':'')+
        '</div></td></tr>';
    }
  });
  if(!list.length) html += '<tr><td colspan="6" class="empty">No queries captured</td></tr>';
  html += '</tbody></table></div></div>';
  $('#page-queries').innerHTML = html;
  var so = $('#slow-only'); if(so) so.addEventListener('change', function(){S._slowOnly=so.checked; renderQueries();});
  var si = $('#q-search'); if(si) si.addEventListener('input', function(){S.qSearch=si.value; renderQueries(); si.focus();});
  $$('#page-queries tr.clickable').forEach(function(r){
    r.addEventListener('click', function(){
      var i = parseInt(r.dataset.idx);
      S.expandedSteps['q'+i] = !S.expandedSteps['q'+i];
      renderQueries();
    });
  });
}

function renderCache(){
  var c = S.cache || {};
  var html = '<div class="cards">'+
    '<div class="card"><div class="label">Driver</div><div class="value" style="font-size:18px">'+(c.driver||'-')+'</div></div>'+
    '<div class="card"><div class="label">Keys</div><div class="value">'+fmtNum(c.keys)+'</div></div>'+
    '<div class="card"><div class="label">Hits</div><div class="value" style="color:var(--green)">'+fmtNum(c.hits)+'</div></div>'+
    '<div class="card"><div class="label">Misses</div><div class="value" style="color:var(--err)">'+fmtNum(c.misses)+'</div></div>'+
    '<div class="card"><div class="label">Hit Rate</div><div class="value" style="color:var(--primary)">'+fmtNum(c.hit_rate*100, 1)+'%</div></div>'+
    '<div class="card"><div class="label">Memory</div><div class="value">'+fmtBytes(c.memory_bytes)+'</div></div>'+
    '</div>';
  html += '<div style="display:flex;gap:10px;margin-bottom:16px">'+
    '<button class="danger" id="cache-clear">Clear Cache</button>'+
    '<input id="cache-prefix" placeholder="Prefix (optional)" style="max-width:240px">'+
    '<button id="cache-clear-prefix">Clear Prefix</button>'+
    '</div>';
  html += '<div class="chart-card"><div class="head"><h3>Hit Ratio Over Time</h3></div><canvas id="chart-cache" style="height:200px"></canvas></div>';
  $('#page-cache').innerHTML = html;
  drawLineChart($('#chart-cache'), S.history.map(function(h){return (h.cache_hit_ratio||0)*100;}), '#3fb950');
  var cc = $('#cache-clear'); if(cc) cc.addEventListener('click', function(){apiPost('cache/clear',{}).then(function(){return api('cache');}).then(function(d){S.cache=d; renderCache();});});
  var cp = $('#cache-clear-prefix'); if(cp) cp.addEventListener('click', function(){apiPost('cache/clear',{prefix:$('#cache-prefix').value}).then(function(){return api('cache');}).then(function(d){S.cache=d; renderCache();});});
}

function renderQueue(){
  var q = S.queue || {};
  var s = q.summary || {};
  var jobs = q.jobs || [];
  var html = '<div class="cards">'+
    '<div class="card"><div class="label">Pending</div><div class="value" style="color:var(--yellow)">'+fmtNum(s.pending)+'</div></div>'+
    '<div class="card"><div class="label">Running</div><div class="value" style="color:var(--primary)">'+fmtNum(s.running)+'</div></div>'+
    '<div class="card"><div class="label">Completed</div><div class="value" style="color:var(--green)">'+fmtNum(s.completed)+'</div></div>'+
    '<div class="card"><div class="label">Failed</div><div class="value" style="color:var(--err)">'+fmtNum(s.failed)+'</div></div>'+
    '</div>';
  html += '<div class="table-wrap"><div class="table-head"><h3>Jobs ('+jobs.length+')</h3></div><div class="table-scroll"><table><thead><tr>'+
    '<th>ID</th><th>Queue</th><th>State</th><th>Attempts</th><th>Queued</th><th>Duration</th><th>Error</th><th>Action</th>'+
    '</tr></thead><tbody>';
  jobs.forEach(function(j){
    var cls = {pending:'yellow', running:'blue', completed:'green', failed:'red'}[j.state]||'gray';
    html += '<tr>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(j.id)+'</td>'+
      '<td>'+escapeHTML(j.queue)+'</td>'+
      '<td><span class="badge '+cls+'">'+j.state+'</span></td>'+
      '<td>'+j.attempts+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+fmtDate(j.queued_at)+'</td>'+
      '<td>'+(j.duration_ms?fmtMS(j.duration_ms):'-')+'</td>'+
      '<td style="font-size:11px;color:var(--err)">'+escapeHTML(j.error||'')+'</td>'+
      '<td>'+(j.state==='failed'?'<button class="retry" data-id="'+j.id+'">Retry</button>':'-')+'</td>'+
      '</tr>';
  });
  if(!jobs.length) html += '<tr><td colspan="8" class="empty">No jobs registered</td></tr>';
  html += '</tbody></table></div></div>';
  $('#page-queue').innerHTML = html;
  $$('.retry').forEach(function(b){
    b.addEventListener('click', function(){apiPost('queue/retry',{id:b.dataset.id}).then(function(){return api('queue');}).then(function(d){S.queue=d; renderQueue();});});
  });
}

function renderScheduler(){
  var tasks = S.tasks || [];
  var html = '<div class="table-wrap"><div class="table-head"><h3>Scheduled Tasks ('+tasks.length+')</h3></div><div class="table-scroll"><table><thead><tr>'+
    '<th>Name</th><th>Cron</th><th>Last Run</th><th>Duration</th><th>Next Run</th><th>Status</th><th>Runs</th><th>Failures</th>'+
    '</tr></thead><tbody>';
  tasks.forEach(function(t){
    var cls = {idle:'gray', running:'blue', failed:'red', ok:'green'}[t.status]||'gray';
    html += '<tr>'+
      '<td><strong>'+escapeHTML(t.name)+'</strong></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(t.cron)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(t.last_run?fmtDate(t.last_run):'-')+'</td>'+
      '<td>'+(t.last_run_ms?fmtMS(t.last_run_ms):'-')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(t.next_run?fmtDate(t.next_run):'-')+'</td>'+
      '<td><span class="badge '+cls+'">'+escapeHTML(t.status||'-')+'</span></td>'+
      '<td>'+fmtNum(t.run_count)+'</td>'+
      '<td>'+(t.fail_count>0?'<span class="badge red">'+t.fail_count+'</span>':'-')+'</td>'+
      '</tr>';
  });
  if(!tasks.length) html += '<tr><td colspan="8" class="empty">No tasks registered</td></tr>';
  html += '</tbody></table></div></div>';
  $('#page-scheduler').innerHTML = html;
}

function renderLogs(){
  var tab = S.logTab;
  var list = (S.logs[tab]||[]).slice().reverse();
  var q = S.logSearch.toLowerCase();
  if(q) list = list.filter(function(e){return (e.message||'').toLowerCase().indexOf(q)>=0;});
  var counts = {app:S.logs.app.length, http:S.logs.http.length, error:S.logs.error.length, panic:S.logs.panic.length, warning:S.logs.warning.length};
  var html = '<div class="log-tabs">';
  [['app','Application'],['http','HTTP'],['error','Errors'],['panic','Panics'],['warning','Warnings']].forEach(function(t){
    html += '<div class="log-tab '+(tab===t[0]?'active':'')+'" data-tab="'+t[0]+'">'+t[1]+'<span class="count">'+counts[t[0]]+'</span></div>';
  });
  html += '</div>';
  html += '<div style="margin-bottom:12px"><input id="log-search" placeholder="Search logs..." value="'+escapeHTML(S.logSearch)+'" style="max-width:400px"></div>';
  html += '<div class="log-viewer">';
  list.slice(0, 500).forEach(function(e){
    html += '<div class="log-line">'+
      '<span class="time">'+fmtTime(e.time)+'</span>'+
      '<span class="level '+tab+'">'+escapeHTML(e.level||tab)+'</span>'+
      '<span class="msg">'+escapeHTML(e.message)+(e.source?'<span style="color:var(--text-muted);margin-left:8px">'+escapeHTML(e.source)+'</span>':'')+'</span>'+
      '</div>';
  });
  if(!list.length) html += '<div class="empty">No log entries</div>';
  html += '</div>';
  $('#page-logs').innerHTML = html;
  $$('.log-tab').forEach(function(t){t.addEventListener('click', function(){S.logTab=t.dataset.tab; renderLogs();});});
  var s = $('#log-search'); if(s) s.addEventListener('input', function(){S.logSearch=s.value; renderLogs(); s.focus();});
}

function renderHealth(){
  var list = S.health || [];
  var green=list.filter(function(h){return h.status==='green';}).length;
  var yellow=list.filter(function(h){return h.status==='yellow';}).length;
  var red=list.filter(function(h){return h.status==='red';}).length;
  var html = '<div class="cards" style="margin-bottom:16px">'+
    '<div class="card"><div class="label">Healthy</div><div class="value" style="color:var(--green)">'+green+'</div></div>'+
    '<div class="card"><div class="label">Warnings</div><div class="value" style="color:var(--yellow)">'+yellow+'</div></div>'+
    '<div class="card"><div class="label">Failing</div><div class="value" style="color:var(--err)">'+red+'</div></div>'+
    '</div>';
  html += '<div class="health-grid">';
  list.forEach(function(h){
    var icon = h.status==='green'?'\u2713':(h.status==='yellow'?'\u26a0':'\u2717');
    html += '<div class="health-card '+h.status+'">'+
      '<div class="indicator">'+icon+'</div>'+
      '<div class="info"><div class="name">'+escapeHTML(h.name)+'</div>'+
      '<div class="msg">'+escapeHTML(h.message||'')+'</div>'+
      '<div class="msg" style="margin-top:4px;font-size:10px">'+fmtDur(h.latency_us)+' \u00b7 '+fmtTime(h.checked)+'</div>'+
      '</div></div>';
  });
  if(!list.length) html += '<div class="empty" style="grid-column:1/-1"><div class="icon">\u2764</div>No health checks registered</div>';
  html += '</div>';
  html += '<div style="margin-top:14px"><button id="health-refresh">Re-run checks</button></div>';
  $('#page-health').innerHTML = html;
  var rb = $('#health-refresh'); if(rb) rb.addEventListener('click', function(){api('health').then(function(d){S.health=d; renderHealth();});});
}

function renderPerformance(){
  var p = S.perf || {};
  var cur = p.current || {};
  var hist = p.history || [];
  var cards = [
    {label:'Goroutines', value:fmtNum(cur.goroutines?cur.goroutines.goroutines:0)},
    {label:'Heap Alloc', value:fmtBytes(cur.heap?cur.heap.alloc:0)},
    {label:'Heap Sys', value:fmtBytes(cur.heap?cur.heap.sys:0)},
    {label:'Stack In Use', value:fmtBytes(cur.stack?cur.stack.in_use:0)},
    {label:'GC Count', value:fmtNum(cur.gc?cur.gc.num_gc:0)},
    {label:'GC Pause', value:fmtDur(cur.gc?cur.gc.pause_ns/1000:0)},
    {label:'Mallocs', value:fmtNum(cur.allocs?cur.allocs.mallocs:0)},
    {label:'Frees', value:fmtNum(cur.allocs?cur.allocs.frees:0)},
    {label:'CPU Usage', value:fmtNum(cur.cpu?cur.cpu.usage_pct:0, 1)+'%'},
    {label:'Mem Sys', value:fmtBytes(cur.memory?cur.memory.sys:0)},
    {label:'Mem Usage', value:fmtNum(cur.memory?cur.memory.usage_pct:0, 1)+'%'},
    {label:'CGO Calls', value:fmtNum(cur.cpu?cur.cpu.cgo_calls:0)},
  ];
  var html = '<div class="cards">';
  cards.forEach(function(c){
    html += '<div class="card"><div class="label">'+c.label+'</div><div class="value">'+c.value+'</div></div>';
  });
  html += '</div>';
  html += '<div class="chart-row">'+
    '<div class="chart-card"><div class="head"><h3>Heap Allocation (MB)</h3></div><canvas id="chart-perf-heap"></canvas></div>'+
    '<div class="chart-card"><div class="head"><h3>Goroutines</h3></div><canvas id="chart-perf-goro"></canvas></div>'+
    '</div>';
  html += '<div class="chart-row">'+
    '<div class="chart-card"><div class="head"><h3>CPU Usage (%)</h3></div><canvas id="chart-perf-cpu"></canvas></div>'+
    '<div class="chart-card"><div class="head"><h3>GC Pauses (ms)</h3></div><canvas id="chart-perf-gc"></canvas></div>'+
    '</div>';
  $('#page-performance').innerHTML = html;
  drawLineChart($('#chart-perf-heap'), hist.map(function(h){return (h.heap_alloc||0)/1024/1024;}), '#bc8cff');
  drawLineChart($('#chart-perf-goro'), hist.map(function(h){return h.goroutines||0;}), '#d29922');
  drawLineChart($('#chart-perf-cpu'), hist.map(function(h){return h.cpu_usage||0;}), '#f85149');
  drawLineChart($('#chart-perf-gc'), hist.map(function(h){return (h.gc_pause_ns||0)/1000/1000;}), '#39d0d8');
}

function renderTimelineList(){
  var list = S.timelines.slice();
  var html = '<div class="table-wrap"><div class="table-head"><h3>Recent Timelines ('+list.length+')</h3></div><div class="table-scroll"><table><thead><tr>'+
    '<th>Time</th><th>Method</th><th>Path</th><th>Status</th><th>Duration</th><th>Steps</th><th>Action</th>'+
    '</tr></thead><tbody>';
  list.slice(0,100).forEach(function(t, i){
    html += '<tr>'+
      '<td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(t.time)+'</td>'+
      '<td><span class="method-pill '+t.method+'">'+t.method+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(t.path)+'</td>'+
      '<td><span class="status-pill '+statusClass(t.status)+'">'+(t.status||'-')+'</span></td>'+
      '<td>'+fmtDur(t.total_us)+'</td>'+
      '<td>'+fmtNum((t.steps||[]).length)+'</td>'+
      '<td><a href="#/timeline" data-id="'+t.id+'">view</a></td>'+
      '</tr>';
  });
  if(!list.length) html += '<tr><td colspan="7" class="empty">No timelines yet</td></tr>';
  html += '</tbody></table></div></div>';
  if(S.expandedTimeline){
    var t = list.find(function(x){return x.id===S.expandedTimeline;});
    if(t){
      html += '<div class="chart-card"><div class="head"><h3>Timeline '+escapeHTML(t.id)+'</h3><button id="tl-close">Close</button></div>';
      html += '<div class="timeline">'+renderTimelineSteps(t.steps||[], 0)+'</div>';
      html += '</div>';
    }
  }
  $('#page-timeline').innerHTML = html;
  $$('a[data-id]').forEach(function(a){
    a.addEventListener('click', function(e){e.preventDefault(); S.expandedTimeline=a.dataset.id; renderTimelineList();});
  });
  var cl = $('#tl-close'); if(cl) cl.addEventListener('click', function(){S.expandedTimeline=null; renderTimelineList();});
  $$('#page-timeline .t-step').forEach(function(s){
    s.addEventListener('click', function(e){
      e.stopPropagation();
      s.classList.toggle('open');
    });
  });
}

function renderTimelineSteps(steps, depth){
  var html = '';
  steps.forEach(function(s){
    var slow = s.duration_us > 100000;
    html += '<div class="t-step '+(slow?'slow':'')+'" style="margin-left:'+(depth*12)+'px">'+
      '<div class="head">'+
      '<span class="name">'+escapeHTML(s.name)+'</span>'+
      '<span class="dur '+(slow?'slow':'')+'">'+fmtDur(s.duration_us)+'</span>'+
      '</div>'+
      '<div class="expand">';
    if(s.metadata){
      for(var k in s.metadata){
        html += '<div><span style="color:var(--primary)">'+escapeHTML(k)+':</span> '+escapeHTML(JSON.stringify(s.metadata[k]))+'</div>';
      }
    }
    html += '<div><span style="color:var(--text-muted)">start:</span> '+fmtTime(s.start)+'</div>';
    html += '<div><span style="color:var(--text-muted)">end:</span> '+fmtTime(s.end)+'</div>';
    html += '</div>';
    if(s.children && s.children.length){
      html += '<div class="children">'+renderTimelineSteps(s.children, depth+1)+'</div>';
    }
    html += '</div>';
  });
  return html;
}

function renderDatabase(){
  var c = $('#page-database');
  var sel = S.dbTableSel;
  var html = '<div class="db-grid">'+
    '<div class="db-tables"><div style="padding:10px 12px;border-bottom:1px solid var(--border);font-size:11px;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim);font-weight:600">Tables ('+S.dbTables.length+')</div>';
  S.dbTables.forEach(function(t, i){
    html += '<div class="item '+(sel===i?'active':'')+'" data-idx="'+i+'">'+
      '<span>'+escapeHTML(t.name)+'</span>'+
      '<span class="count">'+fmtNum(t.rows)+'</span>'+
      '</div>';
  });
  if(!S.dbTables.length) html += '<div class="empty" style="padding:20px">No tables. Set up a DBInspector to enable.</div>';
  html += '</div><div class="db-data">';
  if(sel!=null && S.dbTables[sel]){
    var t = S.dbTables[sel];
    var writable = !!(S.dbData && S.dbData.writable);
    html += '<div class="toolbar"><strong style="font-family:var(--mono)">'+escapeHTML(t.name)+'</strong>'+
      '<input id="db-search" placeholder="Search..." value="'+escapeHTML(S._dbSearch||'')+'" style="max-width:240px">'+
      '<button id="db-refresh">Refresh</button>'+
      (writable ? '<button id="db-new-row">New row</button>' : '')+
      '</div>';
    if(S.dbData){
      var d = S.dbData;
      html += '<div class="table-scroll" style="flex:1;overflow:auto"><table><thead><tr>';
      d.columns.forEach(function(col){
        html += '<th>'+escapeHTML(col.name)+'<br><span style="font-size:9px;text-transform:none;color:var(--text-muted)">'+escapeHTML(col.type)+(col.primary_key?' PK':'')+(col.nullable?'':' NN')+'</span></th>';
      });
      if(d.writable) html += '<th></th>';
      html += '</tr></thead><tbody>';
      if(S._dbNewRow){
        html += '<tr class="db-new-row">';
        d.columns.forEach(function(col){
          html += '<td><input data-col="'+escapeHTML(col.name)+'" style="width:100%" '+(col.primary_key?'placeholder="auto"':'')+'></td>';
        });
        html += '<td><button id="db-new-row-save">Save</button> <button id="db-new-row-cancel">Cancel</button></td></tr>';
      }
      d.rows.forEach(function(row, ri){
        html += '<tr data-ri="'+ri+'">';
        d.columns.forEach(function(col){
          var v = row[col.name];
          var text = v==null?'':String(v);
          if(d.writable && !col.primary_key){
            html += '<td class="db-cell" data-col="'+escapeHTML(col.name)+'" contenteditable="true">'+escapeHTML(text)+'</td>';
          } else {
            html += '<td style="font-size:11px">'+(v==null?'<span style="color:var(--text-muted)">NULL</span>':escapeHTML(text))+'</td>';
          }
        });
        if(d.writable) html += '<td><button class="danger db-delete-row" data-ri="'+ri+'">Delete</button></td>';
        html += '</tr>';
      });
      if(!d.rows.length && !S._dbNewRow) html += '<tr><td colspan="'+(d.columns.length+(d.writable?1:0))+'" class="empty">No rows</td></tr>';
      html += '</tbody></table></div>';
      html += '<div class="pager"><div>Page '+d.page+' of '+Math.max(1, Math.ceil(d.total/d.page_size))+' ('+fmtNum(d.total)+' rows)</div>'+
        '<div><button id="db-prev" '+(d.page<=1?'disabled':'')+'>Prev</button> '+
        '<button id="db-next" '+(d.page>=Math.ceil(d.total/d.page_size)?'disabled':'')+'>Next</button></div></div>';
    } else {
      html += '<div class="empty" style="padding:40px"><div class="icon">&#x25a3;</div>Loading...</div>';
    }
  } else {
    html += '<div class="empty" style="padding:40px"><div class="icon">&#x25a3;</div>Select a table to browse</div>';
  }
  html += '</div></div>';
  c.innerHTML = html;
  $$('.db-tables .item').forEach(function(it){
    it.addEventListener('click', function(){
      S.dbTableSel = parseInt(it.dataset.idx);
      S.dbData = null;
      S.dbPage = 1;
      S._dbNewRow = false;
      renderDatabase();
      loadDBData();
    });
  });
  var ds = $('#db-search'); if(ds) ds.addEventListener('input', function(){S._dbSearch=ds.value;});
  var dr = $('#db-refresh'); if(dr) dr.addEventListener('click', function(){S.dbPage=1; loadDBData();});
  var dp = $('#db-prev'); if(dp) dp.addEventListener('click', function(){S.dbPage--; loadDBData();});
  var dn = $('#db-next'); if(dn) dn.addEventListener('click', function(){S.dbPage++; loadDBData();});
  var dnw = $('#db-new-row'); if(dnw) dnw.addEventListener('click', function(){S._dbNewRow=true; renderDatabase();});
  var dnwc = $('#db-new-row-cancel'); if(dnwc) dnwc.addEventListener('click', function(){S._dbNewRow=false; renderDatabase();});
  var dnws = $('#db-new-row-save'); if(dnws) dnws.addEventListener('click', saveNewDBRow);
  $$('.db-cell').forEach(function(cell){
    cell.addEventListener('blur', onDBCellBlur);
  });
  $$('.db-delete-row').forEach(function(btn){
    btn.addEventListener('click', function(){ deleteDBRow(parseInt(btn.dataset.ri)); });
  });
}
function pkPathFor(row, columns){
  var parts = [];
  columns.forEach(function(col){
    if(col.primary_key) parts.push(encodeURIComponent(col.name)+'='+encodeURIComponent(row[col.name]));
  });
  return parts.join(',');
}
function onDBCellBlur(e){
  var cell = e.target;
  var ri = parseInt(cell.closest('tr').dataset.ri);
  var col = cell.dataset.col;
  var row = S.dbData.rows[ri];
  var newVal = cell.textContent;
  if((row[col]==null ? '' : String(row[col]))===newVal) return;
  var t = S.dbTables[S.dbTableSel];
  var pk = pkPathFor(row, S.dbData.columns);
  var values = {}; values[col] = newVal;
  apiSend('db/tables/'+encodeURIComponent(t.name)+'/rows/'+pk, 'PUT', {values: values}).then(function(res){
    if(!res.ok){ alert('Update failed: '+((res.data&&res.data.error)||res.status)); loadDBData(); return; }
    row[col] = newVal;
  });
}
function deleteDBRow(ri){
  if(!confirm('Delete this row?')) return;
  var row = S.dbData.rows[ri];
  var t = S.dbTables[S.dbTableSel];
  var pk = pkPathFor(row, S.dbData.columns);
  apiSend('db/tables/'+encodeURIComponent(t.name)+'/rows/'+pk, 'DELETE').then(function(res){
    if(!res.ok){ alert('Delete failed: '+((res.data&&res.data.error)||res.status)); return; }
    loadDBData();
  });
}
function saveNewDBRow(){
  var t = S.dbTables[S.dbTableSel];
  var values = {};
  $$('#page-database tr.db-new-row input').forEach(function(input){
    if(input.value!=='') values[input.dataset.col] = input.value;
  });
  apiSend('db/tables/'+encodeURIComponent(t.name)+'/rows', 'POST', {values: values}).then(function(res){
    if(!res.ok){ alert('Insert failed: '+((res.data&&res.data.error)||res.status)); return; }
    S._dbNewRow = false;
    loadDBData();
  });
}
function loadDBData(){
  if(S.dbTableSel==null) return;
  var t = S.dbTables[S.dbTableSel];
  if(!t) return;
  var q = 'db/tables/'+encodeURIComponent(t.name)+'?page='+S.dbPage+'&page_size=50'+(S._dbSearch?'&search='+encodeURIComponent(S._dbSearch):'');
  api(q).then(function(d){S.dbData=d; renderDatabase();}).catch(function(){});
}

// ─── Canvas charts (no external deps) ───────────────────────────────────
function drawLineChart(canvas, data, color){
  if(!canvas) return;
  var dpr = window.devicePixelRatio || 1;
  var w = canvas.clientWidth || 600;
  var h = canvas.clientHeight || 200;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  var ctx = canvas.getContext('2d');
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);

  // Background grid
  ctx.strokeStyle = 'rgba(48,54,61,0.5)';
  ctx.lineWidth = 1;
  ctx.beginPath();
  for(var i=0;i<=4;i++){
    var y = (i/4)*(h-20)+10;
    ctx.moveTo(40, y); ctx.lineTo(w-10, y);
  }
  ctx.stroke();

  if(!data.length){
    ctx.fillStyle = '#6e7681';
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

  // Y-axis labels
  ctx.fillStyle = '#6e7681';
  ctx.font = '10px monospace';
  ctx.textAlign = 'right';
  for(var i=0;i<=4;i++){
    var v = max - (i/4)*(max-min);
    var y = (i/4)*(h-20)+14;
    ctx.fillText(formatLabel(v), 36, y);
  }

  // Line
  ctx.strokeStyle = color || '#58a6ff';
  ctx.lineWidth = 2;
  ctx.beginPath();
  var x0 = 40;
  var w0 = w - 50;
  for(var i=0;i<data.length;i++){
    var x = x0 + (i/Math.max(1,data.length-1))*w0;
    var y = (h-20) - ((data[i]-min)/(max-min))*(h-30) + 10;
    if(i===0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  }
  ctx.stroke();

  // Fill area below line
  ctx.lineTo(x0 + w0, h-10);
  ctx.lineTo(x0, h-10);
  ctx.closePath();
  ctx.fillStyle = (color||'#58a6ff') + '22';
  ctx.fill();
}
function formatLabel(v){
  if(Math.abs(v) >= 1000000) return (v/1000000).toFixed(1)+'M';
  if(Math.abs(v) >= 1000) return (v/1000).toFixed(1)+'k';
  if(Math.abs(v) < 1) return v.toFixed(2);
  return Math.round(v).toString();
}

// ─── Init ──────────────────────────────────────────────────────────────
function init(){
  // Build sidebar nav
  $('.nav').innerHTML = navHTML();
  $$('.nav-item').forEach(function(n){
    n.addEventListener('click', function(){go(n.dataset.page);});
  });

  // Build page containers
  var content = $('.content');
  PAGES.forEach(function(p){
    content.appendChild(el('div', {id:'page-'+p[0], class:'page'}));
  });

  // Initial route — normalize the URL with replaceState only; pushState is
  // reserved for app-initiated navigation (see go()).
  var hash = location.hash.replace(/^#\//,'');
  var initialPage = validPage(hash) ? hash : 'overview';
  render(initialPage);
  if(history.replaceState) history.replaceState(null, '', '#/'+initialPage);

  // Connect WebSocket
  connectWS();

  // Periodic refresh for pages that don't get live updates
  setInterval(function(){
    if(S.page==='routes') api('routes').then(function(d){S.routes=d; if(S.page==='routes') renderRoutes();}).catch(function(){});
    if(S.page==='cache') api('cache').then(function(d){S.cache=d; if(S.page==='cache') renderCache();}).catch(function(){});
    if(S.page==='queue') api('queue').then(function(d){S.queue=d; if(S.page==='queue') renderQueue();}).catch(function(){});
    if(S.page==='scheduler') api('scheduler').then(function(d){S.tasks=d; if(S.page==='scheduler') renderScheduler();}).catch(function(){});
    if(S.page==='logs') {/* logs come via PushLog */}
    if(S.page==='health') api('health').then(function(d){S.health=d; if(S.page==='health') renderHealth();}).catch(function(){});
    if(S.page==='performance') api('performance').then(function(d){S.perf=d; if(S.page==='performance') renderPerformance();}).catch(function(){});
  }, 5000);
}

if(document.readyState==='loading') document.addEventListener('DOMContentLoaded', init);
else init();
})();
`
