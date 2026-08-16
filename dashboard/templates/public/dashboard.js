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
function api(path){
  return fetch(S.base + '/api/' + path).then(function(r){
    if(r.status === 401){
      // Session expired — redirect to login.
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
      if(S.page==='overview') renderOverview();
    } else if(msg.type==='batch'){
      // The hub batches live records into one frame every 100ms. Each
      // entry carries its own channel, so unwrap and dispatch each one.
      if(msg.events && msg.events.length){
        for(var i=0;i<msg.events.length;i++){
          onEvent(msg.events[i].channel, msg.events[i].data);
        }
      }
    } else if(msg.type==='event'){
      onEvent(msg.channel, msg.data);
    }
  };

}
function updateWSIndicator(){
  var dot = $('#ws-dot');
  if(dot) dot.className = 'ws-dot ' + (S.wsConnected?'on':'off');
  var txt = $('#ws-status');
  if(txt) txt.textContent = S.wsConnected?'live':'reconnecting...';
}

// ─── Event dispatch ────────────────────────────────────────────────────
function onEvent(ch, data){
  if(ch==='request'){
    S.requests.push(data);
    if(S.requests.length>500) S.requests.shift();
    if(S.page==='requests') renderRequests();
  } else if(ch==='query'){
    S.queries.push(data);
    if(S.queries.length>300) S.queries.shift();
    if(S.page==='queries') renderQueries();
  } else if(ch==='timeline'){
    S.timelines.unshift(data);
    if(S.timelines.length>50) S.timelines.pop();
    if(S.page==='timeline') renderTimelineList();
  } else if(ch==='event'){
    S.events.unshift(data);
    if(S.events.length>500) S.events.pop();
    if(S.page==='events') renderEvents();
  }
}


// ─── Page initialization ───────────────────────────────────────────────
var PAGES = {
  overview: {title:'Overview', init: initOverview},
  routes: {title:'Routes', init: initRoutes},
  api: {title:'API Explorer', init: initAPI},
  requests: {title:'Live Requests', init: initRequests},
  cache: {title:'Cache', init: initCache},
  logs: {title:'Logs', init: initLogs},
  health: {title:'Health', init: initHealth},
  performance: {title:'Performance', init: initPerformance},
  timeline: {title:'Timeline', init: initTimeline},
  architecture: {title:'Architecture', init: initArchitecture},
  events: {title:'Events', init: initEvents},
  video: {title:'Video Streaming', init: initVideo},
};



function initPage(page){
  S.page = page;
  var titleEl = $('#page-title');
  if(titleEl && PAGES[page]) titleEl.textContent = PAGES[page].title;
  // Update nav active state — this runs on every SPA navigation.
  $$('.nav-item').forEach(function(n){
    n.classList.toggle('active', n.getAttribute('data-nav') === page);
  });
  if(PAGES[page] && PAGES[page].init) PAGES[page].init();
}

// ─── Overview ──────────────────────────────────────────────────────────
function initOverview(){
  if(S.snapshot) renderOverview();
  // Real-time updates are handled by WebSocket snapshots (pushed every 1s).
  // This interval is just a fallback to re-render if WS is disconnected.
  setInterval(function(){
    if(S.page==='overview') renderOverview();
  }, 5000);
}
function renderOverview(){
  var m = S.snapshot ? S.snapshot.metrics : (S.history[S.history.length-1]||{});
  var cards = [
    {label:'Requests Today', value:fmtNum(m.requests_today)},
    {label:'Requests / sec', value:fmtNum(m.requests_per_sec, 1)},
    {label:'Avg Response', value:fmtMS(m.avg_resp_time_ms)},
    {label:'Error Rate', value:fmtNum(m.error_rate*100, 2)+'%', cls:m.error_rate>0.05?'red':''},
    {label:'Active Sessions', value:fmtNum(m.active_sessions)},
    {label:'DB Connections', value:fmtNum(m.db_connections)},
    {label:'Cache Hit', value:fmtNum(m.cache_hit_ratio*100, 1)+'%'},
    {label:'Queue Jobs', value:fmtNum(m.queue_jobs)},
    {label:'Goroutines', value:fmtNum(m.goroutines)},
    {label:'Heap Alloc', value:fmtBytes(m.heap_alloc)},
    {label:'Mem Sys', value:fmtBytes(m.sys)},
    {label:'CPU Usage', value:fmtNum(m.cpu_usage, 1)+'%'},
  ];
  var html = '';
  cards.forEach(function(c){
    html += '<div class="card'+(c.cls?' '+c.cls:'')+'"><div class="label">'+c.label+'</div><div class="value">'+c.value+'</div><div class="delta">live</div></div>';
  });
  var container = $('#overview-cards');
  if(container) container.innerHTML = html;

  drawLineChart($('#chart-rps'), S.history.map(function(h){return h.requests_per_sec||0;}), '#58a6ff');
  drawLineChart($('#chart-latency'), S.history.map(function(h){return h.avg_resp_time_ms||0;}), '#3fb950');
  drawLineChart($('#chart-mem'), S.history.map(function(h){return (h.heap_alloc||0)/1024/1024;}), '#bc8cff');
  drawLineChart($('#chart-goroutines'), S.history.map(function(h){return h.goroutines||0;}), '#d29922');
}

// ─── Routes ────────────────────────────────────────────────────────────
function initRoutes(){
  api('routes').then(function(d){S.routes=d;renderRoutes();});
  var inp = $('#route-search');
  if(inp) inp.addEventListener('input', function(){S._routeSearch=inp.value; renderRoutes(); inp.focus();});
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
    html += '<tr><td><span class="method-pill '+r.method+'">'+r.method+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(r.pattern)+'</td>'+
      '<td>'+fmtNum(r.requests)+'</td><td>'+fmtMS(r.avg_latency_ms)+'</td><td>'+fmtMS(r.max_latency_ms)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(r.last_request?fmtDate(r.last_request):'-')+'</td>'+
      '<td>'+(r.errors>0?'<span class="badge red">'+r.errors+'</span>':'-')+'</td></tr>';
  });
  if(!filtered.length) html = '<tr><td colspan="7" class="empty">No routes registered</td></tr>';
  var tb = $('#routes-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── API Explorer ──────────────────────────────────────────────────────
function initAPI(){
  if(!S.apiRoutes.length){
    api('api-explorer').then(function(d){S.apiRoutes=d;renderAPIExplorer();});
  } else {
    renderAPIExplorer();
  }
}
function renderAPIExplorer(){
  var sel = S.apiRouteSel;
  var c = $('#api-detail');
  if(!c) return;
  var listHtml = '<div style="padding:10px 12px;border-bottom:1px solid var(--border);font-size:11px;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim);font-weight:600">Endpoints ('+S.apiRoutes.length+')</div>';
  S.apiRoutes.forEach(function(r, i){
    listHtml += '<div class="item '+(sel===i?'active':'')+'" data-idx="'+i+'">'+
      '<span class="method-pill '+r.method+'">'+r.method+'</span>'+
      '<span style="font-family:var(--mono);font-size:11px">'+escapeHTML(r.path)+'</span></div>';
  });
  var listEl = $('#api-list');
  if(listEl) listEl.innerHTML = listHtml;
  $$('#api-list .item').forEach(function(it){
    it.addEventListener('click', function(){
      S.apiRouteSel = parseInt(it.dataset.idx);
      S.apiResp = null;
      renderAPIExplorer();
    });
  });

  var html = '';
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
        '</div><div class="tab-content"><button class="copy-btn">Copy</button><pre>'+
        escapeHTML((r2.snippets||{})[S.apiSnippetLang]||'')+
        '</pre></div></div>';
    }
  } else {
    html = '<div class="empty"><div class="icon">\u2197</div>Select an endpoint to begin</div>';
  }
  c.innerHTML = html;

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

// ─── Live Requests ─────────────────────────────────────────────────────
function initRequests(){
  renderRequests();
  ['f-method','f-status','f-route','f-user'].forEach(function(id){
    var e = $('#'+id);
    if(!e) return;
    var evt = id==='f-method'||id==='f-status' ? 'change' : 'input';
    e.addEventListener(evt, function(){
      if(id==='f-method') S.reqFilter.method=e.value;
      if(id==='f-status') S.reqFilter.status=e.value;
      if(id==='f-route') S.reqFilter.route=e.value;
      if(id==='f-user') S.reqFilter.user=e.value;
      renderRequests();
      if(evt==='input') e.focus();
    });
  });
  setInterval(function(){
    if(S.page==='requests') api('requests?limit=200').then(function(d){S.requests=d; renderRequests();}).catch(function(){});
  }, 10000);
}
function renderRequests(){
  var list = S.requests.slice().reverse();
  var f = S.reqFilter;
  if(f.method) list = list.filter(function(r){return r.method===f.method;});
  if(f.status) list = list.filter(function(r){return (''+r.status).startsWith(f.status.replace('xx',''));});
  if(f.route) list = list.filter(function(r){return (r.route||r.path||'').indexOf(f.route)>=0;});
  if(f.user) list = list.filter(function(r){return (r.user||'').toLowerCase().indexOf(f.user.toLowerCase())>=0;});
  var c = $('#req-count');
  if(c) c.textContent = list.length;
  var html = '';
  var max = 200;
  list.slice(0, max).forEach(function(r){
    var durSlow = r.duration_ms > 500;
    html += '<tr><td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(r.time)+'</td>'+
      '<td><span class="method-pill '+r.method+'">'+r.method+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:340px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(r.path)+'</td>'+
      '<td><span class="status-pill '+statusClass(r.status)+'">'+(r.status||'-')+'</span></td>'+
      '<td>'+(durSlow?'<span style="color:var(--err)">'+fmtMS(r.duration_ms)+'</span>':fmtMS(r.duration_ms))+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+escapeHTML(r.ip)+'</td>'+
      '<td style="font-size:11px">'+escapeHTML(r.user||'-')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+fmtBytes(r.resp_size)+'</td>'+
      '<td>'+(r.timeline_id?'<a href="'+S.base+'/timeline" data-tl="'+r.timeline_id+'">view</a>':'-')+'</td></tr>';
  });
  if(list.length>max) html += '<tr><td colspan="9" style="text-align:center;color:var(--text-muted);padding:8px">Showing latest '+max+' of '+list.length+'</td></tr>';
  if(!list.length) html = '<tr><td colspan="9" class="empty">No requests yet</td></tr>';
  var tb = $('#req-tbody');
  if(tb) tb.innerHTML = html;
  $$('a[data-tl]').forEach(function(a){
    a.addEventListener('click', function(e){
      e.preventDefault();
      S._tlSelected = a.dataset.tl;
      if(window.breeze) breeze.navigate(S.base+'/timeline');
      else location.href = S.base+'/timeline';
    });
  });
}

// ─── ORM Queries ───────────────────────────────────────────────────────
function initQueries(){
  renderQueries();
  var so = $('#slow-only');
  if(so) so.addEventListener('change', function(){S._slowOnly=so.checked; renderQueries();});
  var si = $('#q-search');
  if(si) si.addEventListener('input', function(){S.qSearch=si.value; renderQueries(); si.focus();});
  setInterval(function(){
    if(S.page==='queries') api('queries?limit=200').then(function(d){S.queries=d; renderQueries();}).catch(function(){});
  }, 10000);
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
      '<td style="font-family:var(--mono);font-size:11px;max-width:540px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">'+escapeHTML(q.sql)+'</td>'+
      '<td>'+(q.slow?'<span class="badge red">slow</span> ':'')+fmtDur(q.duration_us)+'</td>'+
      '<td style="font-family:var(--mono)">'+fmtNum(q.rows)+'</td>'+
      '<td style="font-family:var(--mono);font-size:10px;color:var(--text-dim)">'+escapeHTML(q.file)+':'+q.line+'</td>'+
      '<td>'+(q.error?'<span class="badge red">error</span>':'<span class="badge green">ok</span>')+'</td></tr>';
  });
  if(!list.length) html = '<tr><td colspan="6" class="empty">No queries captured</td></tr>';
  var tb = $('#q-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── Cache ─────────────────────────────────────────────────────────────
function initCache(){
  loadCache();
  var cc = $('#cache-clear');
  if(cc) cc.addEventListener('click', function(){apiPost('cache/clear',{}).then(loadCache);});
  var cp = $('#cache-clear-prefix');
  if(cp) cp.addEventListener('click', function(){apiPost('cache/clear',{prefix:$('#cache-prefix').value}).then(loadCache);});
  setInterval(function(){ if(S.page==='cache') loadCache(); }, 5000);
}
function loadCache(){
  api('cache').then(function(d){S.cache=d; renderCache();}).catch(function(){});
}
function renderCache(){
  var c = S.cache || {};
  var html = '<div class="card"><div class="label">Driver</div><div class="value" style="font-size:18px">'+(c.driver||'-')+'</div></div>'+
    '<div class="card"><div class="label">Keys</div><div class="value">'+fmtNum(c.keys)+'</div></div>'+
    '<div class="card"><div class="label">Hits</div><div class="value" style="color:var(--green)">'+fmtNum(c.hits)+'</div></div>'+
    '<div class="card"><div class="label">Misses</div><div class="value" style="color:var(--err)">'+fmtNum(c.misses)+'</div></div>'+
    '<div class="card"><div class="label">Hit Rate</div><div class="value" style="color:var(--primary)">'+fmtNum(c.hit_rate*100, 1)+'%</div></div>'+
    '<div class="card"><div class="label">Memory</div><div class="value">'+fmtBytes(c.memory_bytes)+'</div></div>';
  var container = $('#cache-cards');
  if(container) container.innerHTML = html;
  drawLineChart($('#chart-cache'), S.history.map(function(h){return (h.cache_hit_ratio||0)*100;}), '#3fb950');
}

// ─── Queue ─────────────────────────────────────────────────────────────
function initQueue(){
  loadQueue();
  setInterval(function(){ if(S.page==='queue') loadQueue(); }, 5000);
}
function loadQueue(){
  api('queue').then(function(d){S.queue=d; renderQueue();}).catch(function(){});
}
function renderQueue(){
  var q = S.queue || {};
  var s = q.summary || {};
  var jobs = q.jobs || [];
  var cardsHtml = '<div class="card"><div class="label">Pending</div><div class="value" style="color:var(--yellow)">'+fmtNum(s.pending)+'</div></div>'+
    '<div class="card"><div class="label">Running</div><div class="value" style="color:var(--primary)">'+fmtNum(s.running)+'</div></div>'+
    '<div class="card"><div class="label">Completed</div><div class="value" style="color:var(--green)">'+fmtNum(s.completed)+'</div></div>'+
    '<div class="card"><div class="label">Failed</div><div class="value" style="color:var(--err)">'+fmtNum(s.failed)+'</div></div>';
  var cc = $('#queue-cards');
  if(cc) cc.innerHTML = cardsHtml;
  var jc = $('#job-count');
  if(jc) jc.textContent = jobs.length;
  var html = '';
  jobs.forEach(function(j){
    var cls = {pending:'yellow', running:'blue', completed:'green', failed:'red'}[j.state]||'gray';
    html += '<tr><td style="font-family:var(--mono);font-size:11px">'+escapeHTML(j.id)+'</td>'+
      '<td>'+escapeHTML(j.queue)+'</td><td><span class="badge '+cls+'">'+j.state+'</span></td>'+
      '<td>'+j.attempts+'</td><td style="font-size:11px;color:var(--text-dim)">'+fmtDate(j.queued_at)+'</td>'+
      '<td>'+(j.duration_ms?fmtMS(j.duration_ms):'-')+'</td>'+
      '<td style="font-size:11px;color:var(--err)">'+escapeHTML(j.error||'')+'</td>'+
      '<td>'+(j.state==='failed'?'<button class="retry" data-id="'+j.id+'">Retry</button>':'-')+'</td></tr>';
  });
  if(!jobs.length) html = '<tr><td colspan="8" class="empty">No jobs registered</td></tr>';
  var tb = $('#job-tbody');
  if(tb) tb.innerHTML = html;
  $$('.retry').forEach(function(b){
    b.addEventListener('click', function(){apiPost('queue/retry',{id:b.dataset.id}).then(loadQueue);});
  });
}

// ─── Scheduler ─────────────────────────────────────────────────────────
function initScheduler(){
  api('scheduler').then(function(d){S.tasks=d; renderScheduler();}).catch(function(){});
  setInterval(function(){ if(S.page==='scheduler') api('scheduler').then(function(d){S.tasks=d; renderScheduler();}).catch(function(){}); }, 5000);
}
function renderScheduler(){
  var tasks = S.tasks || [];
  var c = $('#task-count');
  if(c) c.textContent = tasks.length;
  var html = '';
  tasks.forEach(function(t){
    var cls = {idle:'gray', running:'blue', failed:'red', ok:'green'}[t.status]||'gray';
    html += '<tr><td><strong>'+escapeHTML(t.name)+'</strong></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(t.cron)+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(t.last_run?fmtDate(t.last_run):'-')+'</td>'+
      '<td>'+(t.last_run_ms?fmtMS(t.last_run_ms):'-')+'</td>'+
      '<td style="font-size:11px;color:var(--text-dim)">'+(t.next_run?fmtDate(t.next_run):'-')+'</td>'+
      '<td><span class="badge '+cls+'">'+escapeHTML(t.status||'-')+'</span></td>'+
      '<td>'+fmtNum(t.run_count)+'</td>'+
      '<td>'+(t.fail_count>0?'<span class="badge red">'+t.fail_count+'</span>':'-')+'</td></tr>';
  });
  if(!tasks.length) html = '<tr><td colspan="8" class="empty">No tasks registered</td></tr>';
  var tb = $('#task-tbody');
  if(tb) tb.innerHTML = html;
}

// ─── Logs ──────────────────────────────────────────────────────────────
function initLogs(){
  $$('.log-tab').forEach(function(t){t.addEventListener('click', function(){
    S.logTab=t.dataset.tab;
    $$('.log-tab').forEach(function(x){x.classList.remove('active')});
    t.classList.add('active');
    // Load the newly-active tab's data.
    api('logs?level='+S.logTab+'&limit=500').then(function(d){S.logs[S.logTab]=d||[]; renderLogs();}).catch(function(){});
    renderLogs();
  });});
  var s = $('#log-search');
  if(s) s.addEventListener('input', function(){S.logSearch=s.value; renderLogs(); s.focus();});
  // Poll only the active tab every 10 seconds (not all 5 tabs).
  setInterval(function(){
    if(S.page==='logs') {
      api('logs?level='+S.logTab+'&limit=500').then(function(d){S.logs[S.logTab]=d||[]; renderLogs();}).catch(function(){});
    }
  }, 10000);
  // Initial load — only the active tab.
  api('logs?level='+S.logTab+'&limit=500').then(function(d){S.logs[S.logTab]=d||[]; renderLogs();}).catch(function(){});
}
function renderLogs(){
  var tab = S.logTab;
  var list = (S.logs[tab]||[]).slice().reverse();
  var q = S.logSearch.toLowerCase();
  if(q) list = list.filter(function(e){return (e.message||'').toLowerCase().indexOf(q)>=0;});
  ['app','http','error','panic','warning'].forEach(function(l){
    var c = $('#log-count-'+l);
    if(c) c.textContent = (S.logs[l]||[]).length;
  });
  var html = '';
  list.slice(0,500).forEach(function(e){
    html += '<div class="log-line"><span class="time">'+fmtTime(e.time)+'</span>'+
      '<span class="level '+tab+'">'+escapeHTML(e.level||tab)+'</span>'+
      '<span class="msg">'+escapeHTML(e.message)+(e.source?'<span style="color:var(--text-muted);margin-left:8px">'+escapeHTML(e.source)+'</span>':'')+'</span></div>';
  });
  if(!list.length) html = '<div class="empty">No log entries</div>';
  var v = $('#log-viewer');
  if(v) v.innerHTML = html;
}

// ─── Health ────────────────────────────────────────────────────────────
function initHealth(){
  loadHealth();
  var rb = $('#health-refresh');
  if(rb) rb.addEventListener('click', loadHealth);
  setInterval(function(){ if(S.page==='health') loadHealth(); }, 5000);
}
function loadHealth(){
  api('health').then(function(d){S.health=d; renderHealth();}).catch(function(){});
}
function renderHealth(){
  var list = S.health || [];
  var green=list.filter(function(h){return h.status==='green';}).length;
  var yellow=list.filter(function(h){return h.status==='yellow';}).length;
  var red=list.filter(function(h){return h.status==='red';}).length;
  var cardsHtml = '<div class="card"><div class="label">Healthy</div><div class="value" style="color:var(--green)">'+green+'</div></div>'+
    '<div class="card"><div class="label">Warnings</div><div class="value" style="color:var(--yellow)">'+yellow+'</div></div>'+
    '<div class="card"><div class="label">Failing</div><div class="value" style="color:var(--err)">'+red+'</div></div>';
  var cc = $('#health-cards');
  if(cc) cc.innerHTML = cardsHtml;
  var html = '';
  list.forEach(function(h){
    var icon = h.status==='green'?'\u2713':(h.status==='yellow'?'\u26a0':'\u2717');
    html += '<div class="health-card '+h.status+'"><div class="indicator">'+icon+'</div>'+
      '<div class="info"><div class="name">'+escapeHTML(h.name)+'</div>'+
      '<div class="msg">'+escapeHTML(h.message||'')+'</div>'+
      '<div class="msg" style="margin-top:4px;font-size:10px">'+fmtDur(h.latency_us)+' \u00b7 '+fmtTime(h.checked)+'</div></div></div>';
  });
  if(!list.length) html = '<div class="empty" style="grid-column:1/-1"><div class="icon">\u2764</div>No health checks registered</div>';
  var g = $('#health-grid');
  if(g) g.innerHTML = html;
}

// ─── Performance ───────────────────────────────────────────────────────
function initPerformance(){
  loadPerf();
  setInterval(function(){ if(S.page==='performance') loadPerf(); }, 5000);
}
function loadPerf(){
  api('performance').then(function(d){S.perf=d; renderPerformance();}).catch(function(){});
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
    {label:'CPU Usage', value:fmtNum(cur.cpu?cur.cpu.usage_pct:0, 1)+'%'},
    {label:'Mem Sys (OS)', value:fmtBytes(cur.memory?cur.memory.sys:0)},
    {label:'Mem Usage %', value:fmtNum(cur.memory?cur.memory.usage_pct:0, 1)+'%'},
    {label:'CGO Calls', value:fmtNum(cur.cpu?cur.cpu.cgo_calls:0)},
    {label:'Num CPU', value:fmtNum(cur.cpu?cur.cpu.num_cpu:0)},
    {label:'GOMAXPROCS', value:fmtNum(cur.cpu?cur.cpu.gomaxprocs:0)},
    {label:'GOGC', value:rt.gogc!==undefined?(rt.gogc===-1?'disabled':String(rt.gogc)):'—', cls:rt.gogc===-1?'red':''},
    {label:'GOMEMLIMIT', value:rt.gomemlimit>0?fmtBytes(rt.gomemlimit):'no limit', cls:rt.gomemlimit<=0?'yellow':''},
  ];
  var html = '';
  cards.forEach(function(c){
    html += '<div class="card'+(c.cls?' '+c.cls:'')+'"><div class="label">'+c.label+'</div><div class="value">'+c.value+'</div></div>';
  });
  var container = $('#perf-cards');
  if(container) container.innerHTML = html;

  // Heap Allocation chart — plots HeapAlloc ONLY (live heap bytes).
  // This DROPS after GC. Never mix with TotalAlloc or HeapSys.
  drawLineChart($('#chart-perf-heap'), hist.map(function(h){return (h.heap_alloc||0)/1024/1024;}), '#bc8cff');

  // Goroutines chart
  drawLineChart($('#chart-perf-goro'), hist.map(function(h){return h.goroutines||0;}), '#d29922');

  // CPU Usage chart
  drawLineChart($('#chart-perf-cpu'), hist.map(function(h){return h.cpu_usage||0;}), '#f85149');

  // GC chart — plots NumGC (GC count) over time.
  // Each step up represents a GC event. This shows ACTUAL GC events
  // instead of a flat line (which is what plotting pause_ns would show,
  // since pause_ns is constant between GCs).
  drawLineChart($('#chart-perf-gc'), hist.map(function(h){return h.num_gc||0;}), '#39d0d8');

  // Memory from OS chart — plots Sys (total bytes from OS).
  // This is separate from HeapAlloc (live heap). Sys grows when Go
  // requests memory from the OS and shrinks when Go returns it
  // (HeapReleased).
  drawLineChart($('#chart-perf-mem-sys'), hist.map(function(h){return (h.sys||0)/1024/1024;}), '#f0883e');

  // GC Pause chart — plots the most recent GC pause duration at each
  // sample. Shows how long the last GC took.
  drawLineChart($('#chart-perf-pause'), hist.map(function(h){return (h.pause_ns||0)/1000/1000;}), '#a371f7');
}

// ─── Timeline ──────────────────────────────────────────────────────────
function initTimeline(){
  api('timeline?limit=50').then(function(d){S.timelines=d; renderTimelineList();}).catch(function(){});
  setInterval(function(){ if(S.page==='timeline') api('timeline?limit=50').then(function(d){S.timelines=d; renderTimelineList();}).catch(function(){}); }, 10000);
}
function renderTimelineList(){
  var list = S.timelines.slice();
  var c = $('#tl-count');
  if(c) c.textContent = list.length;
  var html = '';
  list.slice(0,100).forEach(function(t){
    html += '<tr><td style="font-family:var(--mono);font-size:11px;color:var(--text-dim)">'+fmtTime(t.time)+'</td>'+
      '<td><span class="method-pill '+t.method+'">'+t.method+'</span></td>'+
      '<td style="font-family:var(--mono);font-size:11px">'+escapeHTML(t.path)+'</td>'+
      '<td><span class="status-pill '+statusClass(t.status)+'">'+(t.status||'-')+'</span></td>'+
      '<td>'+fmtDur(t.total_us)+'</td><td>'+fmtNum((t.steps||[]).length)+'</td>'+
      '<td><a href="#" data-id="'+t.id+'">view</a></td></tr>';
  });
  if(!list.length) html = '<tr><td colspan="7" class="empty">No timelines yet</td></tr>';
  var tb = $('#tl-tbody');
  if(tb) tb.innerHTML = html;
  $$('a[data-id]').forEach(function(a){
    a.addEventListener('click', function(e){e.preventDefault(); S._tlSelected=a.dataset.id; renderTimelineDetail();});
  });
  if(S._tlSelected) renderTimelineDetail();
}
function renderTimelineDetail(){
  var t = S.timelines.find(function(x){return x.id===S._tlSelected;});
  var d = $('#tl-detail');
  if(!d || !t) return;
  d.innerHTML = '<div class="chart-card"><div class="head"><h3>Timeline '+escapeHTML(t.id)+'</h3><button id="tl-close">Close</button></div>'+
    '<div class="timeline">'+renderTimelineSteps(t.steps||[], 0)+'</div></div>';
  var cl = $('#tl-close');
  if(cl) cl.addEventListener('click', function(){S._tlSelected=null; d.innerHTML='';});
  $$('#tl-detail .t-step').forEach(function(s){
    s.addEventListener('click', function(e){e.stopPropagation(); s.classList.toggle('open');});
  });
}
function renderTimelineSteps(steps, depth){
  var html = '';
  steps.forEach(function(s){
    var slow = s.duration_us > 100000;
    html += '<div class="t-step '+(slow?'slow':'')+'" style="margin-left:'+(depth*12)+'px">'+
      '<div class="head"><span class="name">'+escapeHTML(s.name)+'</span>'+
      '<span class="dur '+(slow?'slow':'')+'">'+fmtDur(s.duration_us)+'</span></div>'+
      '<div class="expand">';
    if(s.metadata){
      for(var k in s.metadata){
        html += '<div><span style="color:var(--primary)">'+escapeHTML(k)+':</span> '+escapeHTML(JSON.stringify(s.metadata[k]))+'</div>';
      }
    }
    html += '<div><span style="color:var(--text-muted)">start:</span> '+fmtTime(s.start)+'</div>';
    html += '<div><span style="color:var(--text-muted)">end:</span> '+fmtTime(s.end)+'</div>';
    html += '</div>';
    if(s.children && s.children.length) html += '<div class="children">'+renderTimelineSteps(s.children, depth+1)+'</div>';
    html += '</div>';
  });
  return html;
}

// ─── Architecture ─────────────────────────────────────────────────────
var archZoom = 1, archPanX = 0, archPanY = 0;
var archDragging = false, archLastX = 0, archLastY = 0;

function initArchitecture(){
  loadArchitecture();
  setInterval(function(){ if(S.page==='architecture') loadArchitecture(); }, 5000);
  setupArchControls();
}
function loadArchitecture(){
  api('architecture').then(function(d){S.arch=d; renderArchitecture();}).catch(function(){});
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
  if(zIn) zIn.addEventListener('click', function(){ archZoom = Math.min(archZoom*1.2, 3); applyArchTransform(); });
  if(zOut) zOut.addEventListener('click', function(){ archZoom = Math.max(archZoom/1.2, 0.3); applyArchTransform(); });
  if(zReset) zReset.addEventListener('click', function(){ archZoom=1; archPanX=0; archPanY=0; applyArchTransform(); });

  function applyArchTransform(){
    if(vp){
      vp.style.transform = 'translate('+archPanX+'px,'+archPanY+'px) scale('+archZoom+')';
      if(zLabel) zLabel.textContent = Math.round(archZoom*100)+'%';
    }
  }

  // Mouse wheel zoom
  wrap.addEventListener('wheel', function(e){
    e.preventDefault();
    var delta = e.deltaY < 0 ? 1.1 : 1/1.1;
    archZoom = Math.max(0.3, Math.min(archZoom*delta, 3));
    applyArchTransform();
  }, {passive:false});

  // Drag to pan
  wrap.addEventListener('mousedown', function(e){
    archDragging = true;
    archLastX = e.clientX;
    archLastY = e.clientY;
    vp.classList.add('no-transition');
  });
  document.addEventListener('mousemove', function(e){
    if(!archDragging) return;
    archPanX += e.clientX - archLastX;
    archPanY += e.clientY - archLastY;
    archLastX = e.clientX;
    archLastY = e.clientY;
    applyArchTransform();
  });
  document.addEventListener('mouseup', function(){
    if(archDragging){
      archDragging = false;
      if(vp) vp.classList.remove('no-transition');
    }
  });

  // Touch support
  wrap.addEventListener('touchstart', function(e){
    if(e.touches.length===1){
      archDragging = true;
      archLastX = e.touches[0].clientX;
      archLastY = e.touches[0].clientY;
      if(vp) vp.classList.add('no-transition');
    }
  }, {passive:true});
  wrap.addEventListener('touchmove', function(e){
    if(!archDragging || e.touches.length!==1) return;
    archPanX += e.touches[0].clientX - archLastX;
    archPanY += e.touches[0].clientY - archLastY;
    archLastX = e.touches[0].clientX;
    archLastY = e.touches[0].clientY;
    applyArchTransform();
  }, {passive:true});
  wrap.addEventListener('touchend', function(){
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

      var cls = conns[i].status || 'unknown';
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
      var cls = conn.status || 'unknown';
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
    var cls = c.status || 'unknown';
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

function initEvents(){
  loadEvents();
  var s = $('#ev-search');
  if(s) s.addEventListener('input', function(){S.eventFilter.q=s.value; renderEvents(); s.focus();});
  var fo = $('#ev-failed-only');
  if(fo) fo.addEventListener('change', function(){S.eventFilter.failedOnly=fo.checked; renderEvents();});
  var src = $('#ev-source');
  // The source filter is applied server-side, so switching it reloads
  // rather than filtering the page's current slice: the newest 200
  // workflow executions are not necessarily in the newest 200 signals.
  if(src) src.addEventListener('change', function(){S.eventFilter.source=src.value; loadEvents();});
  setInterval(function(){ if(S.page==='events') loadEvents(); }, 10000);
}
function loadEvents(){
  var src = S.eventFilter.source||'';
  api('events?limit=200'+(src?'&source='+encodeURIComponent(src):'')).then(function(d){
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
      tr.addEventListener('click', function(){
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
      if(cl) cl.addEventListener('click', function(){S.eventSel=null; detail.innerHTML='';});
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
function initVideo(){
  loadVideo();
  // Two seconds rather than the ten used elsewhere: bandwidth is the
  // point of this page, and a rate that updates every ten seconds reads
  // as stale while a video is visibly playing.
  setInterval(function(){ if(S.page==='video') loadVideo(); }, 2000);
}
function loadVideo(){
  api('video').then(function(d){ S.video = d; renderVideo(); }).catch(function(){});
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

    html += '<tr>'+
      '<td style="font-family:var(--mono);font-size:11px;max-width:320px;overflow:hidden;text-overflow:ellipsis">'+escapeHTML(f.file)+'</td>'+
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

// ─── Canvas charts ─────────────────────────────────────────────────────
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
  ctx.fillStyle = '#6e7681';
  ctx.font = '10px monospace';
  ctx.textAlign = 'right';
  for(var i=0;i<=4;i++){
    var v = max - (i/4)*(max-min);
    var y = (i/4)*(h-20)+14;
    ctx.fillText(formatLabel(v), 36, y);
  }
  ctx.strokeStyle = color || '#58a6ff';
  ctx.lineWidth = 2;
  ctx.beginPath();
  var x0 = 40, w0 = w - 50;
  for(var i=0;i<data.length;i++){
    var x = x0 + (i/Math.max(1,data.length-1))*w0;
    var y = (h-20) - ((data[i]-min)/(max-min))*(h-30) + 10;
    if(i===0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  }
  ctx.stroke();
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
  // Detect base path from the current URL
  var path = location.pathname;
  var match = path.match(/^(\/[^\/]*dashboard)/);
  S.base = match ? match[1] : '/dashboard';
  connectWS();

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
})();
