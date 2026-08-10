package dashboard

// spaCSS contains all the dashboard's CSS, inlined into the SPA shell so
// the dashboard ships as a single self-contained HTML response.
const spaCSS = `
*{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0d1117;--surface:#161b22;--surface2:#1c2128;--border:#30363d;
  --text:#e6edf3;--text-dim:#8b949e;--text-muted:#6e7681;
  --primary:#58a6ff;--primary-dim:#1f6feb;--accent:#3fb950;--warn:#d29922;
  --err:#f85149;--purple:#bc8cff;--teal:#39d0d8;
  --green:#3fb950;--yellow:#d29922;--red:#f85149;
  --r:8px;--r-sm:4px;--mono:'JetBrains Mono','SF Mono',Menlo,Consolas,monospace;
  --sans:-apple-system,BlinkMacSystemFont,'Segoe UI','Inter',Helvetica,Arial,sans-serif;
}
html,body{height:100%}
body{font-family:var(--sans);background:var(--bg);color:var(--text);font-size:13px;line-height:1.5;overflow:hidden}
a{color:var(--primary);text-decoration:none}
a:hover{color:#7cb6ff}
button{font-family:var(--sans);cursor:pointer;border:1px solid var(--border);background:var(--surface2);color:var(--text);padding:6px 12px;border-radius:var(--r-sm);font-size:12px;transition:all .15s}
button:hover{background:var(--border);border-color:var(--text-dim)}
button.primary{background:var(--primary-dim);border-color:var(--primary-dim);color:#fff}
button.primary:hover{background:var(--primary)}
button.danger{background:transparent;border-color:var(--err);color:var(--err)}
button.danger:hover{background:var(--err);color:#fff}
input,select,textarea{font-family:var(--sans);background:var(--bg);color:var(--text);border:1px solid var(--border);border-radius:var(--r-sm);padding:6px 10px;font-size:12px;outline:none;width:100%}
input:focus,select:focus,textarea:focus{border-color:var(--primary)}
input[type="checkbox"]{width:auto}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--border);white-space:nowrap}
th{font-size:11px;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim);background:var(--surface);position:sticky;top:0;z-index:1}
td{font-size:12px}
tr:hover{background:var(--surface)}
code,pre{font-family:var(--mono);font-size:12px}
pre{background:var(--bg);padding:12px;border-radius:var(--r-sm);overflow:auto;border:1px solid var(--border)}
.badge{display:inline-block;padding:2px 8px;border-radius:12px;font-size:10px;font-weight:600;text-transform:uppercase;letter-spacing:.5px}
.badge.green{background:rgba(63,185,80,.15);color:var(--green);border:1px solid rgba(63,185,80,.3)}
.badge.yellow{background:rgba(210,153,34,.15);color:var(--yellow);border:1px solid rgba(210,153,34,.3)}
.badge.red{background:rgba(248,81,73,.15);color:var(--red);border:1px solid rgba(248,81,73,.3)}
.badge.blue{background:rgba(88,166,255,.15);color:var(--primary);border:1px solid rgba(88,166,255,.3)}
.badge.gray{background:rgba(139,148,158,.15);color:var(--text-dim);border:1px solid rgba(139,148,158,.3)}
.badge.purple{background:rgba(188,140,255,.15);color:var(--purple);border:1px solid rgba(188,140,255,.3)}

/* Layout */
.app{display:grid;grid-template-columns:240px 1fr;height:100vh}
.sidebar{background:var(--surface);border-right:1px solid var(--border);display:flex;flex-direction:column;overflow:hidden}
.sidebar-header{padding:16px 18px;border-bottom:1px solid var(--border);display:flex;align-items:center;gap:10px}
.sidebar-header .logo{width:28px;height:28px;border-radius:6px;background:linear-gradient(135deg,var(--primary),var(--purple));display:flex;align-items:center;justify-content:center;font-weight:700;color:#fff;font-size:14px}
.sidebar-header h1{font-size:14px;font-weight:600;letter-spacing:-.3px}
.sidebar-header .sub{font-size:10px;color:var(--text-muted);text-transform:uppercase;letter-spacing:.5px}
.nav{flex:1;overflow-y:auto;padding:8px 0}
.nav-item{display:flex;align-items:center;gap:10px;padding:8px 18px;color:var(--text-dim);font-size:12px;cursor:pointer;border-left:2px solid transparent;transition:all .12s}
.nav-item:hover{background:var(--surface2);color:var(--text)}
.nav-item.active{color:var(--primary);background:var(--surface2);border-left-color:var(--primary)}
.nav-item .icon{width:14px;height:14px;opacity:.7;flex-shrink:0}
.nav-item.active .icon{opacity:1}
.nav-section{padding:8px 18px 4px;font-size:10px;text-transform:uppercase;color:var(--text-muted);letter-spacing:.6px;font-weight:600}
.sidebar-footer{padding:10px 18px;border-top:1px solid var(--border);font-size:11px;color:var(--text-muted);display:flex;justify-content:space-between;align-items:center}
.ws-dot{width:8px;height:8px;border-radius:50%;display:inline-block;margin-right:6px}
.ws-dot.on{background:var(--green);box-shadow:0 0 6px var(--green)}
.ws-dot.off{background:var(--red)}

/* Main */
.main{display:flex;flex-direction:column;overflow:hidden}
.topbar{height:48px;background:var(--surface);border-bottom:1px solid var(--border);display:flex;align-items:center;justify-content:space-between;padding:0 20px}
.topbar h2{font-size:14px;font-weight:600}
.topbar .right{display:flex;gap:10px;align-items:center;font-size:11px;color:var(--text-dim)}
.content{flex:1;overflow:auto;padding:20px}
.page{display:none}
.page.active{display:block}

/* Cards */
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:14px;margin-bottom:20px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:16px;position:relative;overflow:hidden}
.card .label{font-size:10px;text-transform:uppercase;letter-spacing:.6px;color:var(--text-muted);font-weight:600;margin-bottom:8px;display:flex;align-items:center;gap:6px}
.card .value{font-size:24px;font-weight:600;letter-spacing:-.5px;font-variant-numeric:tabular-nums}
.card .delta{font-size:11px;color:var(--text-dim);margin-top:6px}
.card .spark{position:absolute;bottom:0;left:0;right:0;height:36px;opacity:.7}
.card .icon-bg{position:absolute;top:-8px;right:-8px;font-size:48px;opacity:.06}

/* Charts */
.chart-card{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:16px;margin-bottom:16px}
.chart-card .head{display:flex;justify-content:space-between;align-items:center;margin-bottom:12px}
.chart-card .head h3{font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim)}
.chart-card canvas{width:100%;height:200px;display:block}
.chart-row{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-bottom:16px}
@media(max-width:1100px){.chart-row{grid-template-columns:1fr}}

/* Tables */
.table-wrap{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:hidden}
.table-head{padding:12px 16px;border-bottom:1px solid var(--border);display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap}
.table-head h3{font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim)}
.table-head .filters{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.table-head .filters input,.table-head .filters select{width:auto;min-width:120px}

/* ── Event / workflow detail pane ─────────────────────────────────────
   Shared by event dispatches and workflow executions. A workflow's
   spans are grouped by DAG level, so steps that ran concurrently are
   drawn under one .wf-level header instead of looking sequential. */
.ev-meta{display:flex;flex-wrap:wrap;gap:8px 20px;padding:10px 14px;border-bottom:1px solid var(--border);font-size:12px}
.ev-meta>div{display:flex;gap:6px;align-items:center}
.ev-meta>div>span{color:var(--text-dim);font-size:10px;text-transform:uppercase;letter-spacing:.5px}
.ev-payload{padding:10px 14px;border-bottom:1px solid var(--border)}
.ev-payload>span{color:var(--text-dim);font-size:10px;text-transform:uppercase;letter-spacing:.5px;display:block;margin-bottom:4px}
.ev-payload pre{margin:0;font-family:var(--mono);font-size:11px;white-space:pre-wrap;word-break:break-word;color:var(--text)}
.ev-spans{padding:10px 14px}
.ev-spans .head2{font-size:10px;text-transform:uppercase;letter-spacing:.5px;color:var(--text-dim);margin-bottom:8px;font-weight:600}
.ev-span{display:flex;align-items:center;gap:8px;padding:5px 0;font-size:12px;border-bottom:1px solid rgba(48,54,61,.35)}
.ev-span:last-child{border-bottom:none}
.ev-span.par{padding-left:14px}
.ev-span .idx{color:var(--text-muted);font-family:var(--mono);font-size:10px;min-width:18px}
.ev-span .name{font-family:var(--mono);font-size:11px;min-width:170px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.ev-span .phase,.ev-span .prio{color:var(--text-dim);font-size:10px}
.ev-span .dur{font-family:var(--mono);font-size:11px;min-width:64px;text-align:right;color:var(--text-dim)}
.ev-span .err{color:var(--err);font-size:11px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
/* The bar makes relative step cost readable at a glance; it is scaled
   against the slowest span in the same execution. */
.ev-span .bar{flex:1;min-width:60px;height:6px;background:rgba(48,54,61,.5);border-radius:3px;overflow:hidden}
.ev-span .bar i{display:block;height:100%;border-radius:3px}
.ev-span .bar i.green{background:var(--green)}
.ev-span .bar i.red{background:var(--err)}
.ev-span .bar i.yellow{background:var(--yellow)}
.ev-span .bar i.gray{background:var(--text-muted)}
.wf-level{display:flex;align-items:center;gap:8px;margin:10px 0 4px}
.wf-level .lvl{font-family:var(--mono);font-size:10px;font-weight:700;color:var(--text-dim);background:rgba(48,54,61,.5);padding:1px 6px;border-radius:3px}
.wf-level .tag{font-size:10px;color:var(--text-muted)}
/* A parallel level is called out in colour, because "these ran at the
   same time" is the single most important thing to see here. */
.wf-level.parallel .lvl{color:var(--primary);background:rgba(88,166,255,.15)}
.wf-level.parallel .tag{color:var(--primary)}
.wf-state{font-weight:600;text-transform:capitalize}
.wf-state.completed{color:var(--green)}
.wf-state.failed{color:var(--err)}
.wf-state.compensated,.wf-state.compensating{color:var(--yellow)}
.wf-state.cancelled,.wf-state.timed_out{color:var(--text-muted)}
.wf-state.running,.wf-state.pending{color:var(--primary)}
.table-scroll{max-height:560px;overflow:auto}
.method-pill{display:inline-block;padding:2px 7px;border-radius:3px;font-size:10px;font-weight:700;letter-spacing:.5px;font-family:var(--mono);min-width:42px;text-align:center}
.method-pill.GET{background:rgba(63,185,80,.15);color:var(--green)}
.method-pill.POST{background:rgba(210,153,34,.15);color:var(--yellow)}
.method-pill.PUT{background:rgba(88,166,255,.15);color:var(--primary)}
.method-pill.PATCH{background:rgba(188,140,255,.15);color:var(--purple)}
.method-pill.DELETE{background:rgba(248,81,73,.15);color:var(--red)}
.method-pill.OPTIONS{background:rgba(139,148,158,.15);color:var(--text-dim)}
.status-pill{display:inline-block;padding:2px 7px;border-radius:3px;font-size:10px;font-weight:600;font-family:var(--mono)}
.status-pill.s2{color:var(--green)}
.status-pill.s3{color:var(--primary)}
.status-pill.s4{color:var(--yellow)}
.status-pill.s5{color:var(--red)}
.latency-bar{display:inline-block;width:80px;height:6px;background:var(--bg);border-radius:3px;overflow:hidden;vertical-align:middle;margin-right:8px}
.latency-bar .fill{height:100%;background:linear-gradient(90deg,var(--green),var(--yellow),var(--red))}
.row-detail{background:var(--bg);padding:0;border-bottom:1px solid var(--border)}
.row-detail .inner{padding:14px 16px;border-left:2px solid var(--primary)}

/* API Explorer */
.api-grid{display:grid;grid-template-columns:280px 1fr;gap:16px;height:calc(100vh - 130px)}
.api-list{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:auto}
.api-list .item{padding:8px 12px;border-bottom:1px solid var(--border);cursor:pointer;font-size:11px;display:flex;align-items:center;gap:8px}
.api-list .item:hover{background:var(--surface2)}
.api-list .item.active{background:var(--surface2);border-left:2px solid var(--primary)}
.api-detail{display:flex;flex-direction:column;gap:14px;overflow:auto}
.api-form{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:16px}
.api-form .row{display:grid;grid-template-columns:120px 1fr;gap:10px;margin-bottom:10px;align-items:center}
.api-form .row label{font-size:11px;color:var(--text-dim);text-transform:uppercase;letter-spacing:.5px;font-weight:600}
.api-form .row .controls{display:flex;gap:6px}
.api-form .row .controls select{width:120px}
.api-form .row .controls input{flex:1}
.api-form .headers{display:grid;grid-template-columns:1fr 1fr auto;gap:6px;margin-top:6px}
.api-form .headers .h-row{display:contents}
.api-response{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:hidden}
.api-response .head{padding:10px 14px;border-bottom:1px solid var(--border);display:flex;justify-content:space-between;align-items:center}
.api-response .body{padding:14px;max-height:400px;overflow:auto}
.snippets{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:hidden}
.snippets .tabs{display:flex;border-bottom:1px solid var(--border);overflow-x:auto}
.snippets .tab{padding:8px 14px;font-size:11px;cursor:pointer;border-bottom:2px solid transparent;color:var(--text-dim);font-family:var(--mono)}
.snippets .tab.active{color:var(--primary);border-bottom-color:var(--primary)}
.snippets .tab-content{padding:12px;position:relative}
.snippets .copy-btn{position:absolute;top:8px;right:8px;font-size:10px;padding:4px 8px}

/* Timeline */
.timeline{position:relative;padding-left:24px}
.timeline::before{content:'';position:absolute;left:8px;top:0;bottom:0;width:2px;background:var(--border)}
.t-step{position:relative;padding:8px 0 8px 16px;cursor:pointer}
.t-step::before{content:'';position:absolute;left:-20px;top:14px;width:10px;height:10px;border-radius:50%;background:var(--surface);border:2px solid var(--primary)}
.t-step.slow::before{border-color:var(--err)}
.t-step .head{display:flex;justify-content:space-between;align-items:center;gap:10px}
.t-step .name{font-size:12px;font-weight:500}
.t-step .meta{font-size:11px;color:var(--text-dim);font-family:var(--mono)}
.t-step .dur{font-family:var(--mono);font-size:11px;color:var(--text-dim);background:var(--surface2);padding:1px 6px;border-radius:3px}
.t-step .dur.slow{color:var(--err)}
.t-step .expand{margin-top:8px;display:none;padding:10px;background:var(--bg);border-radius:var(--r-sm);font-family:var(--mono);font-size:11px;color:var(--text-dim);border:1px solid var(--border);white-space:pre-wrap}
.t-step.open .expand{display:block}
.t-step .children{margin-left:12px;padding-left:12px;border-left:1px dashed var(--border)}

/* Health */
.health-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:12px}
.health-card{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:14px;display:flex;align-items:center;gap:12px;border-left-width:3px}
.health-card.green{border-left-color:var(--green)}
.health-card.yellow{border-left-color:var(--yellow)}
.health-card.red{border-left-color:var(--red)}
.health-card .indicator{width:24px;height:24px;border-radius:50%;display:flex;align-items:center;justify-content:center}
.health-card.green .indicator{background:rgba(63,185,80,.15);color:var(--green)}
.health-card.yellow .indicator{background:rgba(210,153,34,.15);color:var(--yellow)}
.health-card.red .indicator{background:rgba(248,81,73,.15);color:var(--red)}
.health-card .info .name{font-size:12px;font-weight:600}
.health-card .info .msg{font-size:11px;color:var(--text-dim);margin-top:2px}

/* DB Browser */
.db-grid{display:grid;grid-template-columns:240px 1fr;gap:16px;height:calc(100vh - 130px)}
.db-tables{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:auto}
.db-tables .item{padding:8px 12px;border-bottom:1px solid var(--border);cursor:pointer;font-size:12px;display:flex;justify-content:space-between;align-items:center}
.db-tables .item:hover{background:var(--surface2)}
.db-tables .item.active{background:var(--surface2);color:var(--primary)}
.db-tables .item .count{font-size:10px;color:var(--text-muted);font-family:var(--mono)}
.db-data{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:hidden;display:flex;flex-direction:column}
.db-data .toolbar{padding:10px 14px;border-bottom:1px solid var(--border);display:flex;gap:10px;align-items:center}
.db-data .pager{padding:10px 14px;border-top:1px solid var(--border);display:flex;justify-content:space-between;align-items:center;font-size:11px;color:var(--text-dim)}

/* Logs */
.log-tabs{display:flex;gap:0;border-bottom:1px solid var(--border);margin-bottom:12px}
.log-tab{padding:8px 14px;font-size:12px;cursor:pointer;border-bottom:2px solid transparent;color:var(--text-dim)}
.log-tab.active{color:var(--primary);border-bottom-color:var(--primary)}
.log-tab .count{font-size:10px;color:var(--text-muted);margin-left:6px}
.log-viewer{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);overflow:hidden;font-family:var(--mono);font-size:11px}
.log-line{padding:6px 12px;border-bottom:1px solid var(--border);display:grid;grid-template-columns:140px 80px 1fr;gap:10px;align-items:start}
.log-line:hover{background:var(--surface2)}
.log-line .time{color:var(--text-muted)}
.log-line .level{text-transform:uppercase;font-weight:600;font-size:10px}
.log-line .level.app{color:var(--primary)}
.log-line .level.http{color:var(--purple)}
.log-line .level.error{color:var(--err)}
.log-line .level.panic{color:var(--red);background:rgba(248,81,73,.1);padding:0 4px;border-radius:2px}
.log-line .level.warning{color:var(--warn)}
.log-line .msg{color:var(--text);white-space:pre-wrap;word-break:break-all}

/* Empty state */
.empty{padding:40px;text-align:center;color:var(--text-muted);font-size:12px}
.empty .icon{font-size:36px;opacity:.3;margin-bottom:10px}

/* Scrollbars */
::-webkit-scrollbar{width:8px;height:8px}
::-webkit-scrollbar-track{background:transparent}
::-webkit-scrollbar-thumb{background:var(--border);border-radius:4px}
::-webkit-scrollbar-thumb:hover{background:var(--text-muted)}

/* Responsive */
@media(max-width:900px){.app{grid-template-columns:1fr}.sidebar{display:none}.api-grid,.db-grid{grid-template-columns:1fr;height:auto}}
`
