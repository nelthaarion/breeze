'use strict';

// Isolated behavioural test for the dashboard's page-scoped polling loop.
// The helper is extracted from dashboard.js so this test does not need to
// boot the entire UI or emulate the dashboard's WebSocket/chart surface.
const fs = require('fs');
const vm = require('vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const start = source.indexOf("// Page-owned event listeners share the page's AbortSignal");
const end = source.indexOf('// ─── Page initialization', start);
const themeStart = source.indexOf('function setTheme(t)');
const apiStart = source.indexOf('// ─── API client');
const apiEnd = source.indexOf('// ─── WebSocket', apiStart);
const themeEnd = source.indexOf('function initTheme()', themeStart);
if (start < 0 || end < 0) throw new Error('page lifecycle block not found');
if (themeStart < 0 || themeEnd < 0) throw new Error('theme redraw function not found');
if (apiStart < 0 || apiEnd < 0) throw new Error('API client block not found');

const failures = [];
function check(name, ok) { if (!ok) failures.push(name); }

let timerID = 0;
const timers = new Map();
const clearedTimers = [];
const cancelledFrames = [];
function fakeSetTimeout(fn, delay) {
  const id = ++timerID;
  timers.set(id, {fn, delay});
  return id;
}
function fakeClearTimeout(id) { clearedTimers.push(id); timers.delete(id); }
function fireNext() {
  const first = timers.entries().next();
  if (first.done) return false;
  const [id, timer] = first.value;
  timers.delete(id);
  timer.fn();
  return true;
}
class FakeAbortController {
  constructor() {
    const listeners = [];
    this._listeners = listeners;
    this.signal = {
      aborted:false,
      addEventListener(type, fn){ if(type === 'abort') listeners.push(fn); },
      removeEventListener(type, fn){ if(type !== 'abort') return; const i=listeners.indexOf(fn); if(i !== -1) listeners.splice(i,1); },
    };
  }
  abort() {
    if (this.signal.aborted) return;
    this.signal.aborted = true;
    this._listeners.forEach(function(fn){ fn(); });
  }
}

class FakeEventTarget {
  constructor(){ this.listeners = new Map(); }
  addEventListener(type, handler, options){
    const list = this.listeners.get(type) || [];
    list.push(handler);
    this.listeners.set(type, list);
    const signal = options && options.signal;
    if(signal && signal.addEventListener){
      const remove = () => this.removeEventListener(type, handler);
      signal.addEventListener('abort', remove, {once:true});
    }
  }
  removeEventListener(type, handler){
    const list = this.listeners.get(type) || [];
    const index = list.indexOf(handler);
    if(index !== -1) list.splice(index, 1);
  }
  dispatch(type){ (this.listeners.get(type) || []).slice().forEach(function(fn){ fn(); }); }
  count(type){ return (this.listeners.get(type) || []).length; }
}

const context = {
  S: {page:'overview'},
  AbortController: FakeAbortController,
  Promise,
  setTimeout: fakeSetTimeout,
  clearTimeout: fakeClearTimeout,
  cancelAnimationFrame: function(id){ cancelledFrames.push(id); },
  _fleetRaf: 0,
  document: {documentElement:{setAttribute:function(){}}},
  localStorage: {setItem:function(){}},
};
vm.createContext(context);
vm.runInContext(source.slice(start, end), context, {filename:'dashboard-poll.js'});

async function run() {
  const pageNode = new FakeEventTarget();
  let oldHandlerCalls = 0;
  let newHandlerCalls = 0;
  context._activePageController = new context.AbortController();
  context.pageOn(pageNode, 'click', function(){ oldHandlerCalls++; });
  context.pageOn(pageNode, 'click', function(){ newHandlerCalls++; });
  check('same-page re-init replaces a retained node handler', pageNode.count('click') === 1);
  pageNode.dispatch('click');
  check('only the current retained-node handler runs', oldHandlerCalls === 0 && newHandlerCalls === 1);
  const listenerController = context._activePageController;
  context.stopPageWork();
  check('navigation aborts page-scoped event listeners', listenerController.signal.aborted === true && pageNode.count('click') === 0);
  pageNode.dispatch('click');
  check('removed page handler cannot fire after navigation', newHandlerCalls === 1);

  let calls = 0;
  let active = 0;
  let maxActive = 0;
  const pending = [];
  context.startPagePoll('overview', function(signal) {
    calls++;
    active++;
    maxActive = Math.max(maxActive, active);
    return new Promise(function(resolve) {
      pending.push(function() { active--; resolve(); });
    });
  }, 25);

  check('first poll is delayed rather than immediate', calls === 0 && timers.size === 1);
  check('timer uses the requested fixed delay', timers.values().next().value.delay === 25);
  fireNext();
  check('first tick runs once', calls === 1 && active === 1);
  check('next tick waits while request is in flight', timers.size === 0);
  check('no overlapping tick can be fired', fireNext() === false && calls === 1);

  pending.shift()();
  await Promise.resolve();
  await Promise.resolve();
  check('next timer is scheduled after completion', timers.size === 1);
  fireNext();
  check('second tick runs after first request completes', calls === 2 && maxActive === 1);

  const activePoll = context._activePagePoll;
  const signal = activePoll.controller.signal;
  context.stopPagePoll();
  check('navigation stop aborts the in-flight request', signal.aborted === true);
  check('stopping clears the poll timer', timers.size === 0);
  pending.shift()();
  await Promise.resolve();
  await Promise.resolve();
  check('a stopped poll does not schedule itself again', timers.size === 0);
  check('poll requests never overlap', maxActive === 1);

  const pageController = new context.AbortController();
  context._activePageController = pageController;
  context.stopPageWork();
  check('navigation cleanup aborts page-owned initial loads', pageController.signal.aborted === true);
  check('navigation cleanup releases the page controller', context._activePageController === null);

  // The shared API client checks abort state after fetch and again after JSON
  // parsing. This closes the race where a response has fulfilled just before
  // navigation aborts the page but its .then callback runs afterward.
  const apiContext={
    S:{base:'/dashboard'}, Promise, Object, JSON, Error,
    window:{location:{href:''}},
    fetch:function(){return Promise.reject(new Error('unexpected fetch'));},
  };
  vm.createContext(apiContext);
  vm.runInContext(source.slice(apiStart, apiEnd), apiContext, {filename:'dashboard-api.js'});
  let resolveLateResponse;
  apiContext.fetch=function(){return new Promise(function(resolve){resolveLateResponse=resolve;});};
  const abortBeforeResponse=new FakeAbortController();
  const lateResponse=apiContext.api('late',{signal:abortBeforeResponse.signal});
  abortBeforeResponse.abort();
  resolveLateResponse({status:200,ok:true,json:function(){return Promise.resolve({stale:true});}});
  let lateError=null;
  try { await lateResponse; } catch(error) { lateError=error; }
  check('API ignores a response fulfilled just before page cancellation', !!lateError && lateError.name==='AbortError');

  let resolveJSON;
  apiContext.fetch=function(){return Promise.resolve({status:200,ok:true,json:function(){return new Promise(function(resolve){resolveJSON=resolve;});}});};
  const abortDuringParse=new FakeAbortController();
  const parsingResponse=apiContext.api('parsing',{signal:abortDuringParse.signal});
  await Promise.resolve();
  abortDuringParse.abort();
  resolveJSON({stale:true});
  let parseError=null;
  try { await parsingResponse; } catch(error) { parseError=error; }
  check('API ignores JSON parsed after page cancellation', !!parseError && parseError.name==='AbortError');

  let postSignal=null;
  apiContext.fetch=function(_url,options){
    postSignal=options.signal;
    return Promise.resolve({status:200,json:function(){return Promise.resolve({ok:true});}});
  };
  const abortPost=new FakeAbortController();
  const postResponse=apiContext.apiPost('mutate',{x:1},{signal:abortPost.signal});
  abortPost.abort();
  let postError=null;
  try { await postResponse; } catch(error) { postError=error; }
  check('API POST forwards its owner signal and discards late completion', postSignal===abortPost.signal && !!postError && postError.name==='AbortError');

  const playbackTimer=991;
  context._fleetRaf=992;
  context.S={page:'fleet',fleet:{playback:{timer:playbackTimer,running:true,speed:0,index:4,finishedAt:Date.now()}}};
  context.archDragging=true;
  context.stopPageWork();
  check('navigation cleanup stops Fleet trace playback', clearedTimers.includes(playbackTimer) && context.S.fleet.playback.timer===null && context.S.fleet.playback.running===false);
  check('navigation cleanup clears the post-playback animation state', context.S.fleet.playback.finishedAt===0);
  check('navigation cleanup cancels the pending Fleet animation frame', cancelledFrames.includes(992) && context._fleetRaf===0);
  check('navigation cleanup resets Architecture drag state', context.archDragging===false);

  // A held 0.0x trace intentionally animates while Fleet is visible, but it
  // must not keep a frame loop alive after navigation to another view.
  const frameStart=source.indexOf('function fleetTopoNeedsFrame(pb)');
  const frameEnd=source.indexOf('function fleetTopoFrame()', frameStart);
  check('Fleet animation predicate source is available', frameStart>=0 && frameEnd>frameStart);
  vm.runInContext(source.slice(frameStart, frameEnd), context, {filename:'dashboard-fleet-animation.js'});
  context.S={page:'overview'};
  check('Fleet frame loop is disabled outside Fleet', context.fleetTopoNeedsFrame({speed:0,index:2,running:true})===false);
  context.S={page:'fleet'};
  check('Fleet 0.0x held playback remains animated while Fleet is active', context.fleetTopoNeedsFrame({speed:0,index:2,running:false})===true);

  // Theme changes are visual-only. They must repaint the current model rather
  // than issue extra API calls for fresh data.
  vm.runInContext(source.slice(themeStart, themeEnd), context, {filename:'dashboard-theme.js'});
  let perfRenders=0, cacheRenders=0, fleetCards=0, fleetTopology=0, fleetTraces=0;
  let redundantFetches=0;
  context.renderPerformance=function(){perfRenders++;};
  context.renderCache=function(){cacheRenders++;};
  context.renderFleetCards=function(services, incidents){fleetCards++; check('fleet theme redraw receives cached data', services.length===1 && incidents.length===1);};
  context.renderFleetTopology=function(topology, services, playback){fleetTopology++; check('fleet topology theme redraw receives cached data', topology.ready===true && services.length===1 && playback===null);};
  context.renderFleetTraces=function(traces){fleetTraces++; check('fleet trace theme redraw receives cached list', traces.length===1);};
  context.loadPerf=function(){redundantFetches++;};
  context.loadCache=function(){redundantFetches++;};
  context.loadFleet=function(){redundantFetches++;};
  context.S={page:'performance'}; context.setTheme('dark');
  context.S={page:'cache'}; context.setTheme('light');
  context.S={page:'fleet',fleet:{services:[{}],incidents:[{}],topology:{ready:true},traces:[{}],playback:null}}; context.setTheme('dark');
  check('theme change redraws performance from cached state', perfRenders===1);
  check('theme change redraws cache from cached state', cacheRenders===1);
  check('theme change redraws fleet cards, topology and traces', fleetCards===1 && fleetTopology===1 && fleetTraces===1);
  check('theme change makes no API reloads', redundantFetches===0);

  // Fleet's four-way load is single-flight too: changing a filter while the
  // previous fetch is pending aborts the old batch, and its finally handler
  // must not clear the loading flag owned by the newer batch.
  const fleetSourceStart = source.indexOf('var _fleetLoadController = null;');
  const fleetSourceEnd = source.indexOf('function renderFleetCards(', fleetSourceStart);
  check('Fleet loader source is available for the regression harness', fleetSourceStart >= 0 && fleetSourceEnd > fleetSourceStart);
  class DeferredAbortController extends FakeAbortController {}
  const fleetEntries=[];
  const fleetContext={
    S:{fleet:{cursor:'',hasMore:false,loading:false,traces:[],topology:null,services:[],incidents:[],playback:null}},
    AbortController:DeferredAbortController, Promise, encodeURIComponent,
    $:function(selector){
      if(selector==='#fleet-tag') return {value:'blue'};
      if(selector==='#fleet-unreachable') return {style:{}};
      if(selector==='#fleet-page-status') return {textContent:''};
      if(selector==='#fleet-next-page') return {style:{}};
      return null;
    },
    api:function(path, options){
      return new Promise(function(resolve,reject){
        const entry={path:path,signal:options.signal,resolve:resolve,reject:reject};
        fleetEntries.push(entry);
        if(options.signal.aborted){const e=new Error('aborted');e.name='AbortError';reject(e);return;}
        options.signal.addEventListener('abort',function(){const e=new Error('aborted');e.name='AbortError';reject(e);},{once:true});
      });
    },
    renderFleetCards:function(){}, renderFleetTopology:function(){}, renderFleetTraces:function(){},
  };
  vm.createContext(fleetContext);
  vm.runInContext(source.slice(fleetSourceStart, fleetSourceEnd), fleetContext, {filename:'dashboard-fleet-loader.js'});
  const fleetPageController=new DeferredAbortController();
  const oldFleetLoad=fleetContext.loadFleet(false,fleetPageController.signal);
  const oldFleetEntries=fleetEntries.slice(0,4);
  const freshFleetLoad=fleetContext.loadFleet(false,fleetPageController.signal);
  check('new Fleet load aborts every request in the superseded batch', oldFleetEntries.length===4 && oldFleetEntries.every(function(entry){return entry.signal.aborted;}));
  check('new Fleet batch owns the loading state', fleetContext.S.fleet.loading===true);
  const currentEntries=fleetEntries.slice(4,8);
  currentEntries.forEach(function(entry){
    if(entry.path==='fleet/services') entry.resolve([{name:'fresh-service'}]);
    else if(entry.path==='fleet/topology') entry.resolve({nodes:[{name:'fresh-service'}]});
    else if(entry.path.indexOf('fleet/traces?')===0) entry.resolve({traces:[{trace_id:'fresh-trace'}],has_more:true,next_cursor:'fresh-cursor'});
    else if(entry.path==='fleet/incidents') entry.resolve([{id:'fresh-incident'}]);
  });
  await freshFleetLoad;
  await oldFleetLoad;
  check('fresh Fleet response wins over the aborted request', fleetContext.S.fleet.services[0].name==='fresh-service' && fleetContext.S.fleet.traces[0].trace_id==='fresh-trace');
  check('Fleet completion stores incidents and clears loading', fleetContext.S.fleet.incidents[0].id==='fresh-incident' && fleetContext.S.fleet.loading===false);

  fleetContext.S.fleet.cursor='page-three-cursor';
  fleetContext.S.fleet.traces=[{trace_id:'page-one-trace'},{trace_id:'page-two-trace'}];
  const refreshFleetLoad=fleetContext.loadFleet(false,fleetPageController.signal,true);
  const refreshEntries=fleetEntries.slice(8,11);
  check('Fleet background refresh skips the paginated trace endpoint', refreshEntries.length===3 && !refreshEntries.some(function(entry){return entry.path.indexOf('fleet/traces?')===0;}));
  refreshEntries.forEach(function(entry){
    if(entry.path==='fleet/services') entry.resolve([{name:'refreshed-service'}]);
    else if(entry.path==='fleet/topology') entry.resolve({nodes:[{name:'refreshed-service'}]});
    else if(entry.path==='fleet/incidents') entry.resolve([{id:'refreshed-incident'}]);
  });
  await refreshFleetLoad;
  check("Fleet background refresh preserves the user page cursor and loaded traces", fleetContext.S.fleet.cursor==='page-three-cursor' && fleetContext.S.fleet.traces.length===2 && fleetContext.S.fleet.traces[1].trace_id==='page-two-trace');
  check('Fleet background refresh updates summary data', fleetContext.S.fleet.services[0].name==='refreshed-service' && fleetContext.S.fleet.incidents[0].id==='refreshed-incident');

  const finalFleetLoad=fleetContext.loadFleet(false,fleetPageController.signal);
  const finalFleetEntries=fleetEntries.slice(11,15);
  fleetPageController.abort();
  check('leaving Fleet aborts the current four-request batch', finalFleetEntries.length===4 && finalFleetEntries.every(function(entry){return entry.signal.aborted;}));
  await finalFleetLoad;
  check('aborted Fleet load releases loading state', fleetContext.S.fleet.loading===false);

  if (failures.length) {
    console.log('FAIL');
    failures.forEach((failure) => console.log('  - ' + failure));
    process.exit(1);
  }
  console.log('PASS');
}

run().catch(function(error) {
  console.log('FAIL\n  - ' + ((error && error.stack) || error));
  process.exit(1);
});
