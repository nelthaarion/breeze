(function () {
   'use strict';

   // ── Lifecycle hooks ────────────────────────────────────────────────────
   //
   // Breeze.onBeforeNavigate(fn) / onAfterNavigate(fn)
   // Breeze.onBeforeSubmit(fn)   / onAfterSubmit(fn)
   //
   // fn receives a detail object:
   //   onBeforeNavigate({ url })          — return false to cancel
   //   onAfterNavigate({ url })
   //   onBeforeSubmit({ form, url })      — return false to cancel
   //   onAfterSubmit({ form, url, html })
   //
   // Multiple handlers can be registered; they run in registration order.

   var _hooks = {
      beforeNavigate: [],
      afterNavigate:  [],
      beforeSubmit:   [],
      afterSubmit:    [],
   };

   function _runHooks(name, detail) {
      var list = _hooks[name];
      for (var i = 0; i < list.length; i++) {
         if (list[i](detail) === false) return false;
      }
      return true;
   }

   // ── Loading state ──────────────────────────────────────────────────────

   function _loadingStart() { document.body.classList.add('breeze-loading'); }
   function _loadingEnd()   { document.body.classList.remove('breeze-loading'); }

   // ── Smart script execution ─────────────────────────────────────────────
   //
   // Rules (applied inside swap() after every fragment insert):
   //
   //   External scripts (src="..."):
   //     Execute only once. A module-level Set tracks loaded URLs.
   //     If already loaded, the <script> element is removed so it doesn't
   //     appear in the DOM twice but its side-effects are not repeated.
   //
   //   Inline scripts (no src):
   //     data-spa-run="always" → execute on every swap (the explicit opt-in).
   //     data-spa-run="once"   → execute once per page lifecycle (tracked by Set).
   //     no attribute          → never re-execute after the initial page load.
   //     The attribute is checked case-insensitively.
   //
   //   Module scripts (type="module"):
   //     Always external-style de-duplication for src modules.
   //     Inline modules execute only on initial page load (browser de-dupes them
   //     anyway for the src case; we honour the same for inline).
   //
   //   Execution order:
   //     Scripts are processed in document order (forEach preserves DOM order).
   //     Each eligible script is cloned and appended; the old node is removed.

   // URLs of external scripts already executed (survives across navigations).
   var _loadedScripts = new Set();
   // IDs/src of inline once-scripts already run (keyed by data-spa-id or content hash).
   var _onceScripts   = new Set();

   // Simple djb2-style hash for content-keying inline once-scripts.
   function _hashStr(s) {
      var h = 5381;
      for (var i = 0; i < s.length; i++) h = (h * 33) ^ s.charCodeAt(i);
      return (h >>> 0).toString(36);
   }

   // Execute scripts inside el after a swap. el is the element whose innerHTML
   // was just replaced. Scripts already in the document head are not affected.
   function _runScripts(el) {
      var scripts = el.querySelectorAll('script');
      scripts.forEach(function (old) {
         var src     = old.getAttribute('src');
         var type    = (old.getAttribute('type') || '').toLowerCase().trim();
         var spaRun  = (old.getAttribute('data-spa-run') || '').toLowerCase().trim();
         var isModule = type === 'module';

         // ── External script ──────────────────────────────────────────────
         if (src) {
         var key = src;
         if (_loadedScripts.has(key)) {
            // Already loaded — remove the stale node, do not re-execute.
            old.parentNode && old.parentNode.removeChild(old);
            return;
         }
         _loadedScripts.add(key);
         var s = document.createElement('script');
         Array.from(old.attributes).forEach(function (a) { s.setAttribute(a.name, a.value); });
         old.parentNode.replaceChild(s, old);
         return;
         }

         // ── Inline script ────────────────────────────────────────────────
         var content = old.textContent || '';

         if (spaRun === 'always') {
         // Explicit always — execute unconditionally.
         var s = document.createElement('script');
         Array.from(old.attributes).forEach(function (a) { s.setAttribute(a.name, a.value); });
         s.textContent = content;
         old.parentNode.replaceChild(s, old);
         return;
         }

         if (spaRun === 'once') {
         var key = old.getAttribute('data-spa-id') || _hashStr(content);
         if (_onceScripts.has(key)) {
            old.parentNode && old.parentNode.removeChild(old);
            return;
         }
         _onceScripts.add(key);
         var s = document.createElement('script');
         Array.from(old.attributes).forEach(function (a) { s.setAttribute(a.name, a.value); });
         s.textContent = content;
         old.parentNode.replaceChild(s, old);
         return;
         }

         // No data-spa-run attribute (including inline modules):
         // Remove from DOM; do not execute. The initial page-load already ran it.
         old.parentNode && old.parentNode.removeChild(old);
      });
   }

   // ── Utilities ──────────────────────────────────────────────────────────

   function resolveEl(target) {
      if (!target) return document.getElementById('breeze-app') ||
                           document.querySelector('main') ||
                           document.body;
      if (typeof target === 'string') return document.querySelector(target);
      return target;
   }

   // Swap innerHTML of el, then run scripts with smart de-duplication.
   //
   // _refreshStateTags runs FIRST, before _runScripts, for two reasons:
   //   1. It relocates any __breeze_data__/__breeze_tmpl__/__breeze_i18n__
   //      tags found in the fragment out to <body>, removing them from el's
   //      subtree. _runScripts treats any <script> without a data-spa-run
   //      attribute as a stale inline script and deletes it — running it
   //      first would strip these JSON tags before they could ever be read.
   //   2. Any content script in the fragment that calls breeze.* at its own
   //      top level (e.g. breeze.bind(...)) must see this route's data, not
   //      whatever the previously displayed route left behind.
   function swap(el, html) {
      el.innerHTML = html;
      _refreshStateTags(el);
      _runScripts(el);
   }

   // ── Core fetch helper ──────────────────────────────────────────────────

   async function breezeGet(url, target, options) {
      options = options || {};
      var el = resolveEl(target);

      try {
         var res = await fetch(url, {
         method:      options.method  || 'GET',
         body:        options.body    || undefined,
         credentials: 'same-origin',
         headers: Object.assign(
            { 'X-Breeze-Fragment': 'true' },
            options.headers || {}
         ),
         });

         var html = await res.text();

         if (!res.ok) {
         if (options.onError) { options.onError(res, el); }
         return html;
         }

         if (el) { swap(el, html); }
         if (options.onSuccess) { options.onSuccess(html, el); }

         window.dispatchEvent(new CustomEvent('breeze:update', {
         detail: { url: url, target: target, html: html }
         }));

         return html;
      } catch (e) {
         if (options.onError) { options.onError(e, el); }
         throw e;
      }
   }

   // ── Polling ────────────────────────────────────────────────────────────

   var _polls = new WeakMap();

   function breezePoll(url, target, intervalMs, options) {
      var el = resolveEl(target);
      if (!el) { console.warn('breeze.poll: target not found', target); return; }
      breezeStop(el);
      breezeGet(url, el, options);
      var id = setInterval(function () { breezeGet(url, el, options); }, intervalMs || 5000);
      _polls.set(el, id);
   }

   function breezeStop(target) {
      var el = resolveEl(target);
      if (!el) return;
      var id = _polls.get(el);
      if (id !== undefined) { clearInterval(id); _polls.delete(el); }
   }

   // ── SPA navigation ─────────────────────────────────────────────────────

   // Take manual control of scroll restoration. The browser's native
   // restoration runs synchronously on popstate — before our async fragment
   // fetch resolves — which would restore scroll against stale content. We
   // restore scroll ourselves once the new content is in place instead.
   if ('scrollRestoration' in history) {
      try { history.scrollRestoration = 'manual'; } catch (e) {}
   }

   function getAppTarget() {
      return document.getElementById('breeze-app') ||
            document.querySelector('main') ||
            document.body;
   }

   // Locale this page was rendered with ('' when i18n is not enabled).
   // _pageLocale is hoisted; the i18n tag is injected before this script.
   var _pageLang = _pageLocale();

   // Persists the current scroll offset onto the *current* history entry so
   // it can be restored later if the user navigates back/forward to it.
   // Called continuously (debounced) while scrolling, and once more right
   // before any programmatic navigation, so the saved value is always fresh
   // regardless of which path the user takes away from the page.
   function _saveScroll() {
      try {
         var s = (history.state && typeof history.state === 'object') ? history.state : {};
         history.replaceState(
         Object.assign({}, s, { scrollY: window.scrollY }),
         '',
         window.location.pathname + window.location.search
         );
      } catch (e) {}
   }

   var _scrollSaveTimer = null;
   window.addEventListener('scroll', function () {
      if (_scrollSaveTimer) return;
      _scrollSaveTimer = setTimeout(function () {
         _scrollSaveTimer = null;
         _saveScroll();
      }, 150);
   }, { passive: true });

   // Caches successful partial-fragment responses by normalized URL so that
   // revisiting an already-fetched route (e.g. A -> B -> A, or Back/Forward)
   // reuses the cached HTML instead of re-fetching. Failed responses are
   // never cached. In-memory only, capped to avoid unbounded growth on
   // long-lived sessions.
   var _routeCache    = new Map();
   var _routeCacheMax = 50;

   function _normalizeUrl(url) {
      try {
         var u = new URL(url, window.location.origin);
         return u.pathname + u.search;
      } catch (e) {
         return url;
      }
   }

   function _cacheRoute(key, html) {
      if (_routeCache.has(key)) _routeCache.delete(key); // refresh recency
      _routeCache.set(key, html);
      if (_routeCache.size > _routeCacheMax) {
         _routeCache.delete(_routeCache.keys().next().value); // evict oldest
      }
   }

   // Invalidation helpers. Wired into breeze.setData()/the reactive store and
   // into non-GET SPA form submissions (see below), so that any client- or
   // server-side state mutation forces the *next* visit to a route — via
   // link, back/forward, or history restore — to re-fetch instead of
   // replaying a pre-mutation snapshot from _routeCache. This is the fix for
   // "go back and the page shows stale/broken content": the cache had no
   // eviction path tied to writes, so it kept serving fragments that no
   // longer matched server (or client store) state.
   function _invalidateRoute(url) {
      _routeCache.delete(_normalizeUrl(url));
   }

   function _invalidateCache() {
      _routeCache.clear();
   }

   // The route cache is process memory holding rendered HTML, so it has to
   // respect what the server said about storing that HTML. A response marked
   // no-store/no-cache, or private (per-user), was previously cached anyway
   // and then replayed on the next visit to the same URL, showing one user's
   // rendered page from memory after the server had explicitly asked for it
   // not to be kept.
   function _isCacheable(res) {
      if (!res || !res.headers) return true;
      var cc = (res.headers.get('Cache-Control') || '').toLowerCase();
      if (!cc) return true;
      return !(/\bno-store\b/.test(cc) || /\bno-cache\b/.test(cc) || /\bprivate\b/.test(cc));
   }

   // Identity of the current session, as reported by the server. Cached
   // fragments belong to whoever was signed in when they were fetched, so a
   // login, logout, or account switch has to drop them: otherwise the next
   // user of this tab could be served the previous user's pages straight from
   // memory, without a request the server could have refused.
   var _authEpoch = null;

   function _syncAuthEpoch(res) {
      if (!res || !res.headers) return;
      var epoch = res.headers.get('X-Breeze-Auth');
      if (epoch === null) return;
      if (_authEpoch !== null && epoch !== _authEpoch) _invalidateCache();
      _authEpoch = epoch;
   }


   // Monotonic sequence guard: if a newer navigation starts while an older
   // one is still in flight, the older response is discarded on arrival so
   // out-of-order fetches can never clobber newer content (duplicate/stale
   // rendering) or push a stale history entry. Paired with an AbortController
   // so the superseded request is actually cancelled, not just ignored.
   var _navSeq       = 0;
   var _navController = null;

   async function navigate(url, push) {
      // Before hook — returning false cancels navigation.
      if (_runHooks('beforeNavigate', { url: url }) === false) return;

      // Cancel any navigation request still in flight — its response would
      // be discarded anyway, so stop wasting bandwidth/CPU on it.
      if (_navController) _navController.abort();
      var controller = new AbortController();
      _navController = controller;

      // "once" inline scripts (data-spa-run="once") are scoped to a single
      // navigation/mount, not to the whole page lifetime: every real
      // navigation tears down and rebuilds the DOM via innerHTML, so any
      // listeners a once-script attached directly to elements (rather than
      // via delegation) are gone and must be reattached when the view is
      // (re)entered — including via the back/forward button. Clearing here
      // (and not inside breezeGet/poll/_rerender) means frequent polling or
      // partial re-renders still only run a once-script a single time.
      _onceScripts.clear();

      var seq = ++_navSeq;
      var key = _normalizeUrl(url);
      _loadingStart();
      try {
         var html = _routeCache.get(key);

         if (html === undefined) {
         var res = await fetch(url, {
            headers: { 'X-Breeze-Partial': 'true' },
            credentials: 'same-origin',
            signal: controller.signal,
         });

         if (!res.ok) { window.location.href = url; return; }

         // Checked before anything is stored, so a login or logout that
         // lands on this response clears the previous identity's pages
         // before this one is itself considered for caching.
         _syncAuthEpoch(res);

         // A response in a different language than the page was rendered
         // with means the locale changed (e.g. a ?lang= switch). Fall back
         // to a full page load so the embedded i18n dictionary, template
         // sources, and route cache are all rebuilt for the new locale.
         var lang = res.headers.get('Content-Language') || '';
         if (lang && _pageLang && lang !== _pageLang) {
            window.location.href = url;
            return;
         }

         html = await res.text();
         if (_isCacheable(res)) _cacheRoute(key, html);
         }

         // A newer navigation started while this fetch was in flight — drop
         // this stale response rather than render/push it out of order.
         if (seq !== _navSeq) return;

         var target = getAppTarget();
         swap(target, html);

         if (push) { history.pushState({ breezeUrl: url, scrollY: 0 }, '', url); }

         window.dispatchEvent(new CustomEvent('breeze:navigate', { detail: { url: url } }));

         // Restore/reset scroll only after the browser has finished laying out
         // the newly swapped DOM (next frame), so we don't scroll against
         // stale geometry from before the swap.
         requestAnimationFrame(function () {
         if (push) {
            window.scrollTo(0, 0);
         } else {
            var restoreY = (history.state && typeof history.state.scrollY === 'number')
               ? history.state.scrollY : 0;
            window.scrollTo(0, restoreY);
         }
         });

         _runHooks('afterNavigate', { url: url });
      } catch (e) {
         // A superseded navigation's request was aborted on purpose — that is
         // not a network failure, so it must never trigger the error fallback.
         if (e && e.name === 'AbortError') return;
         window.location.href = url;
      } finally {
         if (_navController === controller) _navController = null;
         if (seq === _navSeq) _loadingEnd();
      }
   }

   document.addEventListener('click', function (e) {
      // A click target is not always an Element: it can be a text node, and
      // only Elements have closest(). Calling it blindly threw inside the
      // listener, taking the whole delegated click path down with it.
      if (!e.target || e.target.nodeType !== 1) return;
      var a = e.target.closest('a[href]');
      if (!a) return;
      var href = a.getAttribute('href');
      if (!href) return;
      if (
         a.hasAttribute('data-no-spa') || a.hasAttribute('target') ||
         href.startsWith('http') || href.startsWith('//') ||
         href.startsWith('#')    || href.startsWith('mailto:') ||
         href.startsWith('tel:')
      ) return;

      e.preventDefault();
      var targetUrl   = new URL(href, window.location.origin);
      var targetFull  = targetUrl.pathname + targetUrl.search;
      var currentFull = window.location.pathname + window.location.search;
      // Skip only when the destination is truly identical (path AND query) —
      // comparing pathname alone let same-path-same-query clicks through and
      // push a duplicate history entry for the URL already on screen.
      if (targetFull === currentFull) return;
      _saveScroll();
      navigate(href, true);
   });

   window.addEventListener('popstate', function () {
      navigate(window.location.pathname + window.location.search, false);
   });

   if (!history.state || !history.state.breezeUrl) {
      history.replaceState(
         { breezeUrl: window.location.pathname + window.location.search, scrollY: window.scrollY },
         '',
         window.location.pathname + window.location.search
      );
   }

   // ── SPA form handling ──────────────────────────────────────────────────
   //
   // Intercepts form submits unless:
   //   - target="_blank"
   //   - enctype="multipart/form-data" (file uploads — let browser handle)
   //   - data-spa="false" (explicit opt-out)
   //   - action is an external URL (different origin)
   //   - the form has a [download] attribute (unusual but guard it)
   //
   // GET forms:
   //   Serialise with URLSearchParams, navigate via SPA router.
   //
   // POST forms:
   //   Use fetch(). Content-Type negotiation:
   //     application/x-www-form-urlencoded (default)
   //     application/json (if form has data-content-type="application/json")
   //     text/plain       (if form has data-content-type="text/plain")
   //   Response HTML is swapped into #breeze-app exactly like a navigation.
   //   History is pushed with the form's action URL.
   //
   // Progressive enhancement: if JS is disabled the form submits normally.

   document.addEventListener('submit', function (e) {
      var form = e.target;
      if (!form || form.tagName !== 'FORM') return;

      // ── Opt-out conditions ───────────────────────────────────────────────
      if (form.getAttribute('data-spa') === 'false') return;
      if (form.getAttribute('target') === '_blank')  return;
      if ((form.getAttribute('enctype') || '').toLowerCase() === 'multipart/form-data') return;

      var rawAction = form.getAttribute('action') || window.location.pathname;
      // External action → let browser handle.
      try {
         var actionUrl = new URL(rawAction, window.location.origin);
         if (actionUrl.origin !== window.location.origin) return;
      } catch (_) { return; }

      e.preventDefault();

      var method = (form.getAttribute('method') || 'GET').toUpperCase();
      var data   = new FormData(form);

      // Before hook — returning false cancels submission.
      if (_runHooks('beforeSubmit', { form: form, url: rawAction }) === false) return;

      _loadingStart();

      if (method === 'GET') {
         // GET: serialise to query string and navigate.
         var params = new URLSearchParams();
         data.forEach(function (val, key) { params.append(key, val); });
         // The fragment stays on the URL the user ends up at, per the HTML
         // spec: only the query is replaced by the serialised form data.
         var qs  = params.toString();
         var url = actionUrl.pathname + (qs ? '?' + qs : '') + actionUrl.hash;
         _saveScroll();
         navigate(url, true).finally(_loadingEnd);
         return;
      }

      // POST (and PUT/PATCH/DELETE via method override if present).
      var contentType = (form.getAttribute('data-content-type') || '').toLowerCase().trim();
      var body, ct;

      if (contentType === 'application/json') {
         // Convert FormData to a plain object for JSON serialisation.
         var obj = {};
         data.forEach(function (val, key) {
         if (Object.prototype.hasOwnProperty.call(obj, key)) {
            if (!Array.isArray(obj[key])) obj[key] = [obj[key]];
            obj[key].push(val);
         } else {
            obj[key] = val;
         }
         });
         body = JSON.stringify(obj);
         ct   = 'application/json';
      } else if (contentType === 'text/plain') {
         var lines = [];
         data.forEach(function (val, key) { lines.push(key + '=' + val); });
         body = lines.join('\n');
         ct   = 'text/plain';
      } else {
         // Default: application/x-www-form-urlencoded.
         body = new URLSearchParams(data).toString();
         ct   = 'application/x-www-form-urlencoded';
      }

      // The action query string is part of the endpoint, not decoration:
      // POST /items?list=2 addresses a different resource than POST /items,
      // and dropping it sent every such form to the wrong place. Only the
      // fragment is left off the request, since it is never sent to a
      // server; it is kept for the URL the user ends up looking at.
      var actionPath = actionUrl.pathname + actionUrl.search;
      var actionHref = actionPath + actionUrl.hash;

      _saveScroll();

      fetch(actionPath, {
         method:      method,
         credentials: 'same-origin',
         headers: {
         'Content-Type':     ct,
         'X-Breeze-Partial': 'true',
         },
         body: body,
      }).then(function (res) {
         return res.text().then(function (html) {
         if (!res.ok) {
            // Server error — fall back to a real navigation so the user sees it.
            window.location.href = actionHref;
            return;
         }
         var target = getAppTarget();
         // A non-GET submit is inherently a mutation — any other cached
         // route may now be stale (e.g. a list page this POST just added
         // a row to), so drop the whole cache rather than guess which
         // entries are affected. The response we just got is re-cached
         // fresh immediately after.
         _invalidateCache();
         _syncAuthEpoch(res);
         swap(target, html);
         if (_isCacheable(res)) _cacheRoute(_normalizeUrl(actionPath), html);
         history.pushState({ breezeUrl: actionHref, scrollY: 0 }, '', actionHref);
         window.scrollTo(0, 0);
         window.dispatchEvent(new CustomEvent('breeze:navigate', { detail: { url: actionPath } }));
         _runHooks('afterSubmit', { form: form, url: actionPath, html: html });
         });
      }).catch(function () {
         // Network failure — fall back to normal browser submit.
         //
         // Taken from the prototype: a control named or id'd "submit" shadows
         // form.submit with the element itself, so calling it directly throws
         // instead of submitting, losing the user's input at the exact moment
         // this fallback exists to save it.
         HTMLFormElement.prototype.submit.call(form);
      }).finally(function () {
         _loadingEnd();
      });
   });

   // ── Reactive data store ────────────────────────────────────────────────
   //
   // breeze.data() used to be a one-shot read of the page's embedded JSON:
   // fine for rendering the initial view, but mutating an array or object
   // returned from it (push/splice/property assignment/delete) was
   // invisible to the framework — nothing re-rendered and nothing
   // invalidated the cached route, so the UI silently drifted out of sync
   // with the data. _store is now wrapped in a Proxy so that any mutation,
   // however it's made, is observable: it notifies breeze.watch()
   // callbacks, re-renders any region registered with breeze.bind(), and
   // invalidates the current route's cache entry (see _onStoreChange).

   var _store       = null;
   var _watchers    = [];
   var _bindings    = []; // { target, name, key } registered via breeze.bind()
   var _renderTimer = null;

   var _mutatingArrayMethods = ['push', 'pop', 'shift', 'unshift', 'splice', 'sort', 'reverse', 'copyWithin', 'fill'];

   // Wraps an object/array in a Proxy that calls onChange after any
   // mutation: direct property/index writes, deletes, or array mutator
   // methods. Nested objects/arrays are wrapped lazily on read, so the
   // whole tree stays reactive without an eager deep walk.
   //
   // One Proxy per underlying object, kept in a WeakMap. Wrapping on every
   // read handed out a brand new Proxy each time, so data().user !== the
   // same expression a line later: identity comparisons failed, and anything
   // keyed on a value from the store (a WeakMap, a memo, a React-style dep
   // array) treated every read as a new object. The map is weak, so entries
   // disappear with the objects they wrap.
   var _proxyCache = new WeakMap();

   function _makeReactive(value, onChange) {
      if (value === null || typeof value !== 'object' || value.__breezeReactive__) return value;

      var hit = _proxyCache.get(value);
      if (hit !== undefined && hit.fn === onChange) return hit.proxy;

      var proxy = new Proxy(value, {
         get: function (target, prop, receiver) {
         if (prop === '__breezeReactive__') return true;
         var val = Reflect.get(target, prop, receiver);
         if (Array.isArray(target) && typeof val === 'function' && _mutatingArrayMethods.indexOf(prop) !== -1) {
            return function () {
               var result = val.apply(target, arguments);
               onChange();
               return result;
            };
         }
         return (val !== null && typeof val === 'object') ? _makeReactive(val, onChange) : val;
         },
         set: function (target, prop, val, receiver) {
         // Assigning the value a property already holds is not a change.
         // Reporting it as one re-rendered every bound region and woke every
         // watcher, so idle code that reassigns the same value in a loop or
         // on an interval kept the page busy re-rendering identical output.
         var had  = Object.prototype.hasOwnProperty.call(target, prop);
         var prev = had ? target[prop] : undefined;
         var ok   = Reflect.set(target, prop, val, receiver);
         if (ok && (!had || !Object.is(prev, val))) onChange();
         return ok;
         },
         deleteProperty: function (target, prop) {
         // Likewise, deleting something that was never there changes nothing.
         var had = Object.prototype.hasOwnProperty.call(target, prop);
         var ok  = Reflect.deleteProperty(target, prop);
         if (ok && had) onChange();
         return ok;
         },
      });

      _proxyCache.set(value, { proxy: proxy, fn: onChange });
      return proxy;
   }

   // Debounced (via setTimeout 0) so several synchronous mutations in the
   // same tick — e.g. a loop of list.add() calls — collapse into a single
   // re-render/watcher pass instead of one per mutation.
   function _onStoreChange() {
      if (_renderTimer) return;
      _renderTimer = setTimeout(function () {
         _renderTimer = null;
         _invalidateRoute(window.location.pathname + window.location.search);
         _notifyWatchers(_store);
         _renderBindings();
      }, 0);
   }

   // Set for the duration of one binding pass. Bindings asking for the same
   // template with the same data share a single server render through it;
   // see _rerender. Null outside a pass, so a direct breeze.render() call is
   // never folded into anything else.
   var _renderBatch = null;

   function _renderBindings() {
      var outer = _renderBatch;
      _renderBatch = new Map();
      try {
         for (var i = 0; i < _bindings.length; i++) {
            var b = _bindings[i];
            var data = b.key ? (_store ? _store[b.key] : undefined) : _store;
            _rerender(b.name, data, b.target);
         }
      } finally {
         _renderBatch = outer;
      }
   }

   function _readDataTag() {
      var el = document.getElementById('__breeze_data__');
      if (!el) return {};
      try { return JSON.parse(el.textContent); } catch(e) { return {}; }
   }

   function _notifyWatchers(data) {
      for (var i = 0; i < _watchers.length; i++) {
         try { _watchers[i](data); } catch(e) { console.error('breeze.watch callback error', e); }
      }
   }

   // Called by swap() after every fragment insert (navigation, breeze.fetch,
   // poll, render, form submit). If the fragment carries fresh
   // __breeze_data__ / __breeze_i18n__ / __breeze_tmpl__ tags — every view
   // now embeds these, see execView on the server — the corresponding
   // client-side caches are dropped and the store is rehydrated from the
   // new tag. Previously these tags were only ever injected into full page
   // loads and lived outside #breeze-app, so breeze.data() silently froze
   // at whatever the very first page load contained and never reflected
   // the route actually on screen after an SPA navigation.
   function _refreshStateTags(el) {
      var sawData = false;
      ['__breeze_data__', '__breeze_i18n__', '__breeze_tmpl__'].forEach(function (id) {
         var fresh = el.querySelector('#' + id);
         if (!fresh) return;
         var existing = document.getElementById(id);
         if (existing && existing !== fresh) existing.parentNode.removeChild(existing);
         // Relocate to <body> so exactly one canonical instance exists and it
         // survives later swaps that don't happen to touch this tag.
         document.body.appendChild(fresh);
         if (id === '__breeze_data__') sawData = true;
      });

      _i18nCache = null;
      _tmplCache = null;

      if (sawData) {
         _store = _makeReactive(_readDataTag(), _onStoreChange);
         _notifyWatchers(_store);
         _renderBindings();
      }
   }

   // ── Client-side i18n ───────────────────────────────────────────────────
   //
   // The server injects the active locale's flattened dictionary as a
   // non-executing JSON tag (id __breeze_i18n__, data-locale attribute) so
   // the client-side evaluator can resolve {{t "key"}} during re-renders.

   var _i18nCache = null;

   function _i18nDict() {
      if (_i18nCache) return _i18nCache;
      var el = document.getElementById('__breeze_i18n__');
      if (!el) { _i18nCache = {}; return _i18nCache; }
      try { _i18nCache = JSON.parse(el.textContent); } catch(e) { _i18nCache = {}; }
      return _i18nCache;
   }

   function _pageLocale() {
      var el = document.getElementById('__breeze_i18n__');
      return el ? (el.getAttribute('data-locale') || '') : '';
   }

   // Tokenize the argument list of a t tag: quoted strings become literals,
   // everything else is an expression (number or dot-path).
   function _tTokens(s) {
      var tokens = [];
      var i = 0;
      while (i < s.length) {
         var c = s[i];
         if (c === ' ' || c === '\t') { i++; continue; }
         if (c === '"' || c === "'") {
         var end = s.indexOf(c, i + 1);
         if (end === -1) { tokens.push({ lit: s.slice(i + 1) }); break; }
         tokens.push({ lit: s.slice(i + 1, end) });
         i = end + 1;
         } else {
         var j = i;
         while (j < s.length && s[j] !== ' ' && s[j] !== '\t') j++;
         tokens.push({ expr: s.slice(i, j) });
         i = j;
         }
      }
      return tokens;
   }

   // Evaluate a {{t "key" ...args}} tag against the embedded dictionary,
   // mirroring the server-side semantics: "count" selects a zero/one/other
   // plural form, %{name} placeholders interpolate from the args, and a
   // missing key echoes the key itself.
   function _evalT(rest, ctx) {
      var tokens = _tTokens(rest);
      if (!tokens.length || tokens[0].lit === undefined) return '';
      var key = tokens[0].lit;
      var dict = _i18nDict();

      var args = {};
      for (var i = 1; i + 1 < tokens.length; i += 2) {
         var name = tokens[i].lit !== undefined ? tokens[i].lit : tokens[i].expr;
         var vt = tokens[i + 1];
         var argVal; // NOT named val — var hoists, and it must not shadow the lookup below
         if (vt.lit !== undefined) {
         argVal = vt.lit;
         } else if (vt.expr !== '' && !isNaN(Number(vt.expr))) {
         argVal = Number(vt.expr);
         } else {
         argVal = _resolvePath(vt.expr, ctx);
         }
         args[name] = argVal;
      }

      var val;
      if (Object.prototype.hasOwnProperty.call(args, 'count')) {
         var n = Number(args['count']);
         if (n === 0 && dict[key + '.zero'] !== undefined) val = dict[key + '.zero'];
         else if (n === 1 && dict[key + '.one'] !== undefined) val = dict[key + '.one'];
         else if (dict[key + '.other'] !== undefined) val = dict[key + '.other'];
      }
      if (val === undefined) val = dict[key];
      if (val === undefined) return key;

      return val.replace(/%\{([^}]+)\}/g, function (m, name) {
         return Object.prototype.hasOwnProperty.call(args, name) ? String(args[name]) : m;
      });
   }

   // ── Client-side Go-template evaluator ──────────────────────────────────

   var _tmplCache = null;

   function _tmplSources() {
      if (_tmplCache) return _tmplCache;
      var el = document.getElementById('__breeze_tmpl__');
      if (!el) { _tmplCache = {}; return _tmplCache; }
      try { _tmplCache = JSON.parse(el.textContent); } catch(e) { _tmplCache = {}; }
      return _tmplCache;
   }

   function _resolvePath(path, ctx) {
      var p = path.trim();
      if (p === '.' || p === '') return ctx;
      if (p[0] === '.') p = p.slice(1);
      var parts = p.split('.');
      var val = ctx;
      for (var i = 0; i < parts.length; i++) {
         if (val == null) return undefined;
         val = val[parts[i]];
      }
      return val;
   }

   function _evalTmpl(src, ctx, sources, depth) {
      depth = depth || 0;
      if (depth > 16) return '';

      var out = '';
      var pos = 0;

      while (pos < src.length) {
         var open = src.indexOf('{{', pos);
         if (open === -1) { out += src.slice(pos); break; }
         out += src.slice(pos, open);

         var close = src.indexOf('}}', open + 2);
         if (close === -1) { out += src.slice(open); break; }

         var tag = src.slice(open + 2, close).trim();
         pos = close + 2;

         if (tag.slice(0, 2) === '/*') continue;

         if (tag.slice(0, 5) === 'range') {
         var rangePath = tag.slice(5).trim();
         var block = _extractBlock(src, pos, 'range');
         pos = block.end;
         var items = _resolvePath(rangePath, ctx);
         if (Array.isArray(items)) {
            for (var ri = 0; ri < items.length; ri++) {
               out += _evalTmpl(block.body, items[ri], sources, depth + 1);
            }
         } else if (items && typeof items === 'object') {
            var keys = Object.keys(items);
            for (var ki = 0; ki < keys.length; ki++) {
               out += _evalTmpl(block.body, items[keys[ki]], sources, depth + 1);
            }
         }
         continue;
         }

         if (tag.slice(0, 2) === 'if') {
         var ifExpr = tag.slice(2).trim();
         var negate = false;
         if (ifExpr.slice(0, 3) === 'not') { negate = true; ifExpr = ifExpr.slice(3).trim(); }
         var block = _extractBlock(src, pos, 'if');
         pos = block.end;
         var val = _resolvePath(ifExpr, ctx);
         var truthy = !!(Array.isArray(val) ? val.length : val);
         if (negate) truthy = !truthy;
         if (truthy) out += _evalTmpl(block.body, ctx, sources, depth + 1);
         continue;
         }

         if (tag === 'end') continue;

         if (tag.slice(0, 2) === 't ') {
         out += _evalT(tag.slice(2), ctx);
         continue;
         }

         if (tag.slice(0, 9) === 'component' || tag.slice(0, 7) === 'partial') {
         var rest = tag.slice(tag[0] === 'c' ? 9 : 7).trim();
         var q = rest[0];
         if (q === '"' || q === "'") {
            var nameEnd  = rest.indexOf(q, 1);
            var compName = rest.slice(1, nameEnd);
            var dataExpr = rest.slice(nameEnd + 1).trim();
            var compData = dataExpr ? _resolvePath(dataExpr, ctx) : ctx;
            var compSrc  = sources[compName];
            if (compSrc !== undefined) {
               out += _evalTmpl(compSrc, compData, sources, depth + 1);
            }
         }
         continue;
         }

         if (tag[0] === '.') {
         var resolved = _resolvePath(tag, ctx);
         if (resolved != null) out += String(resolved);
         continue;
         }
      }

      return out;
   }

   function _extractBlock(src, pos, tag) {
      var depth = 1;
      var body  = '';
      var i     = pos;

      while (i < src.length) {
         var open  = src.indexOf('{{', i);
         if (open === -1) break;
         var close = src.indexOf('}}', open + 2);
         if (close === -1) break;

         var inner = src.slice(open + 2, close).trim();

         if (inner.slice(0, tag.length) === tag) {
         depth++;
         body += src.slice(i, close + 2);
         i = close + 2;
         } else if (inner === 'end') {
         depth--;
         if (depth === 0) { body += src.slice(i, open); return { body: body, end: close + 2 }; }
         body += src.slice(i, close + 2);
         i = close + 2;
         } else {
         body += src.slice(i, close + 2);
         i = close + 2;
         }
      }

      return { body: body, end: i };
   }

   function _rerender(name, data, target) {
      var el      = resolveEl(target);
      var sources = _tmplSources();

      if (sources[name] !== undefined) {
         var html = _evalTmpl(sources[name], data, sources, 0);
         if (el) { swap(el, html); }
         window.dispatchEvent(new CustomEvent('breeze:render', {
         detail: { name: name, target: target, html: html, local: true }
         }));
         return Promise.resolve(html);
      }

      function postRender(body) {
         return fetch('/breeze/render', {
         method: 'POST',
         credentials: 'same-origin',
         headers: { 'Content-Type': 'application/json' },
         body: JSON.stringify(body),
         });
      }

      // Ten regions bound to one template and one object used to fire ten
      // identical POSTs for a single state change, each returning the same
      // HTML. Within a binding pass they share one request; the reply is
      // swapped into every target separately, so each still renders itself.
      var pending = null;
      if (_renderBatch !== null) {
         var hit = _renderBatch.get(name);
         if (hit !== undefined && hit.data === data) pending = hit.promise;
      }

      if (pending === null) {
         pending = postRender({ component: name, data: data })
            .then(function(res) {
            if (res.status === 400) return postRender({ view: name, data: data });
            return res;
            })
            .then(function(res) {
            return res.text().then(function(html) {
               return { ok: res.ok, html: html };
            });
            });
         if (_renderBatch !== null) _renderBatch.set(name, { data: data, promise: pending });
      }

      return pending.then(function(out) {
         if (!out.ok) { console.error('breeze.render error:', out.html); return out.html; }
         if (el) { swap(el, out.html); }
         window.dispatchEvent(new CustomEvent('breeze:render', {
            detail: { name: name, target: target, html: out.html, local: false }
         }));
         return out.html;
      });
   }

   // ── Public API ─────────────────────────────────────────────────────────

   window.Breeze = {
      // Lifecycle hooks — register before navigation/submit events fire.
      //
      // Breeze.onBeforeNavigate(fn({ url }))     — return false to cancel
      // Breeze.onAfterNavigate(fn({ url }))
      // Breeze.onBeforeSubmit(fn({ form, url })) — return false to cancel
      // Breeze.onAfterSubmit(fn({ form, url, html }))
      //
      // Example:
      //   Breeze.onBeforeNavigate(function(e) {
      //     console.log('navigating to', e.url);
      //   });
      onBeforeNavigate: function(fn) { _hooks.beforeNavigate.push(fn); },
      onAfterNavigate:  function(fn) { _hooks.afterNavigate.push(fn);  },
      onBeforeSubmit:   function(fn) { _hooks.beforeSubmit.push(fn);   },
      onAfterSubmit:    function(fn) { _hooks.afterSubmit.push(fn);    },
   };

   window.breeze = {
      fetch:    breezeGet,
      poll:     breezePoll,
      stop:     breezeStop,
      swap:     function(target, html) { swap(resolveEl(target), html); },
      navigate: function(url) { navigate(url, true); },

      data: function(key) {
         if (_store === null) _store = _makeReactive(_readDataTag(), _onStoreChange);
         return key ? (_store && typeof _store === 'object' ? _store[key] : undefined) : _store;
      },

      setData: function(newData, target, name) {
         _store = _makeReactive(newData, _onStoreChange);
         _invalidateRoute(window.location.pathname + window.location.search);
         _notifyWatchers(_store);
         _renderBindings();
         if (name) { return _rerender(name, _store, target); }
         return Promise.resolve(_store);
      },

      render: function(name, data, target) {
         return _rerender(name, data !== undefined ? data : _store, target);
      },

      watch: function(fn) {
         _watchers.push(fn);
         return function() { _watchers = _watchers.filter(function(w) { return w !== fn; }); };
      },

      // ── Reactive collections ──────────────────────────────────────────────
      //
      // breeze.list('items') returns helpers over breeze.data().items that
      // mutate the array in place (push/splice/index assignment). Because the
      // store is Proxy-backed, every one of these calls automatically:
      //   - notifies breeze.watch() callbacks
      //   - re-renders any region registered with breeze.bind()
      //   - invalidates the current route's cache entry
      // — no manual breeze.render()/breeze.setData() call needed afterwards.
      list: function(key) {
         var d = window.breeze.data();
         if (!Array.isArray(d[key])) d[key] = [];
         return {
         all:    function()      { return d[key]; },
         add:    function(item)  { d[key].push(item); return d[key]; },
         update: function(index, patch) {
            var i = (typeof index === 'function') ? d[key].findIndex(index) : index;
            if (i !== -1 && d[key][i] !== undefined) {
               d[key][i] = Object.assign({}, d[key][i], patch);
            }
            return d[key];
         },
         remove: function(index) {
            var i = (typeof index === 'function') ? d[key].findIndex(index) : index;
            if (i !== -1) d[key].splice(i, 1);
            return d[key];
         },
         set: function(arr) { d[key] = arr; return d[key]; },
         };
      },

      // Registers a persistent binding: whenever the store (or store[key], if
      // given) changes — via setData, breeze.list(), or direct mutation of
      // breeze.data() — target is automatically re-rendered with the
      // client-side template 'name'. Renders once immediately with current
      // data. Returns an unbind function.
      bind: function(target, name, key) {
         var b = { target: target, name: name, key: key };
         _bindings.push(b);
         var data = key ? window.breeze.data(key) : window.breeze.data();
         _rerender(name, data, target);
         return function unbind() { _bindings = _bindings.filter(function(x) { return x !== b; }); };
      },

      // Force the next visit to 'url' (default: the current route) to
      // re-fetch instead of reusing a cached fragment. invalidateAll() clears
      // every cached route. Called automatically on any store mutation and
      // on non-GET SPA form submissions; exposed here for manual use when a
      // mutation happens through some path the framework can't see (e.g. a
      // fetch() call you make yourself).
      invalidate:    function(url) { _invalidateRoute(url || (window.location.pathname + window.location.search)); },
      invalidateAll: function()    { _invalidateCache(); },

      // Delegated event binding: attaches once to document and matches
      // selector via closest() on every event, so handlers keep working
      // for elements that get replaced by a later swap — unlike
      // el.addEventListener() calls made inside a view's own inline script,
      // which lose their target the moment that view's content is swapped
      // out and back in. Returns a function that removes the listener.
      on: function(selector, event, handler) {
         var listener = function(e) {
         // A target is not always an Element: clicks land on text nodes, and
         // events retargeted from document or window have no closest() at
         // all. Walk up to the nearest element first so a click on a
         // button's own label still matches, instead of throwing here.
         var node = e.target;
         if (node && node.nodeType !== 1) node = node.parentElement;
         if (!node || typeof node.closest !== 'function') return;
         var el = node.closest(selector);
         if (el) handler.call(el, e, el);
         };
         document.addEventListener(event, listener);
         return function off() { document.removeEventListener(event, listener); };
      },

      ws: function(path, handlers) {
         handlers = handlers || {};
         var delay = 1000, maxDelay = 30000, stopped = false, socket = null;
         var proto = location.protocol === 'https:' ? 'wss' : 'ws';
         var url   = proto + '://' + location.host + path;

         function connect() {
         socket = new WebSocket(url);

         socket.addEventListener('open', function(e) {
            delay = 1000;
            if (handlers.onOpen) handlers.onOpen(e);
            window.dispatchEvent(new CustomEvent('breeze:ws:open', { detail: { path: path } }));
         });

         socket.addEventListener('message', function(e) {
            if (handlers.onMessage) handlers.onMessage(e);
            window.dispatchEvent(new CustomEvent('breeze:ws:message', {
               detail: { data: e.data, path: path }
            }));
         });

         socket.addEventListener('close', function(e) {
            if (handlers.onClose) handlers.onClose(e);
            window.dispatchEvent(new CustomEvent('breeze:ws:close', {
               detail: { path: path, code: e.code }
            }));
            if (!stopped) {
               setTimeout(connect, delay);
               delay = Math.min(delay * 2, maxDelay);
            }
         });

         socket.addEventListener('error', function(e) {
            if (handlers.onError) handlers.onError(e);
         });
         }

         connect();

         return {
         send:  function(msg) {
            if (socket && socket.readyState === WebSocket.OPEN) socket.send(msg);
         },
         close: function() { stopped = true; if (socket) socket.close(); },
         get socket() { return socket; },
         };
      },
   };
})();
