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
   // External scripts are loaded once per effective URL/type/security tuple.
   // The registry stores the in-flight Promise, not a boolean: concurrent
   // navigations share one request, and a failed request is evicted so a later
   // navigation can retry it. Executed script elements are removed after load
   // so route changes cannot leave an ever-growing collection of script tags.
   //
   // Inline scripts run only with an explicit data-spa-run="always|once".
   // Non-JavaScript script elements (JSON, JSON-LD, import maps, etc.) are data
   // and are left untouched. Scripts are processed in document order; an
   // external script is awaited before the next eligible script is processed.

   var _scriptRegistry = new Map();
   var _onceScripts = new Set();
   var _scriptRunTokens = new WeakMap();

   function _hashStr(s) {
      var h = 5381;
      for (var i = 0; i < s.length; i++) h = (h * 33) ^ s.charCodeAt(i);
      return (h >>> 0).toString(36);
   }

   function _scriptType(type) {
      // MIME parameters do not change a script's execution model.
      return (type || '').toLowerCase().split(';', 1)[0].trim();
   }

   function _isExecutableScript(type) {
      // Import maps and JSON data types deliberately remain non-executable
      // and are not removed by the runner.
      type = _scriptType(type);
      if (!type || type === 'module') return true;
      return /^(?:text|application)\/(?:java|ecma)script$/.test(type) ||
             /^(?:text|application)\/x-(?:java|ecma)script$/.test(type) ||
             type === 'text/jscript';
   }

   function _scriptURL(src) {
      try {
         var url = new URL(src, document.baseURI || window.location.href);
         // URL fragments are not sent in HTTP requests and do not distinguish
         // script resources; excluding them avoids loading the same file twice.
         url.hash = '';
         return url.href;
      } catch (e) { return src; }
   }

   function _scriptKey(script, src) {
      var type = _scriptType(script.getAttribute('type'));
      // All executable classic-JavaScript MIME aliases share one execution
      // model. Modules remain separate because their scope/dependency semantics
      // differ from classic scripts.
      type = type === 'module' ? 'module' : 'classic';
      return [
         type,
         src,
         script.getAttribute('integrity') || '',
         (script.getAttribute('crossorigin') || '').toLowerCase(),
         (script.getAttribute('referrerpolicy') || '').toLowerCase(),
      ].join('\u0000');
   }

   function _cloneScript(old) {
      var fresh = document.createElement('script');
      Array.from(old.attributes).forEach(function (a) { fresh.setAttribute(a.name, a.value); });
      // The HTML parser recognizes the module execution mode by its exact
      // keyword, not by a MIME value with parameters. _scriptType accepts the
      // normalized essence for consistent routing/registry identity, so make
      // the actual replacement node match that same execution model.
      if (_scriptType(old.getAttribute('type')) === 'module') {
         fresh.setAttribute('type', 'module');
      }
      fresh.textContent = old.textContent || '';
      return fresh;
   }

   function _removeNode(node) {
      if (node && node.parentNode) node.parentNode.removeChild(node);
   }

   function _reportScriptError(el, script, error) {
      console.error('Breeze script load failed:', script, error);
      window.dispatchEvent(new CustomEvent('breeze:scripterror', {
         detail: { target: el, src: script.getAttribute('src') || '', error: error }
      }));
   }

   function _loadExternalScript(old, target, isCurrent) {
      var src = _scriptURL(old.getAttribute('src') || '');
      var key = _scriptKey(old, src);

      function load() {
         var pending = _scriptRegistry.get(key);
         if (pending) {
            return pending.then(function () {
               _removeNode(old);
            }, function (error) {
               // A concurrent navigation requested the same URL while its
               // first load was in flight. An active waiter gets one real
               // retry; a superseded route must not spend bandwidth retrying.
               if (isCurrent && !isCurrent()) throw error;
               return load();
            });
         }

         var fresh = _cloneScript(old);
         // Dynamic classic scripts are async by default. We intentionally
         // serialize route scripts in source order, so opt out of that default.
         if (_scriptType(old.getAttribute('type')) !== 'module') {
            fresh.async = false;
            fresh.removeAttribute && fresh.removeAttribute('async');
         }

         var parent = document.head || document.documentElement || document.body;
         var request = new Promise(function (resolve, reject) {
            fresh.onload = function () { resolve(); };
            fresh.onerror = function () { reject(new Error('Unable to load ' + src)); };
            parent.appendChild(fresh);
         }).then(function () {
            _removeNode(fresh);
         }, function (error) {
            _removeNode(fresh);
            throw error;
         });

         _scriptRegistry.set(key, request);
         // Failed entries must not poison future navigation attempts.
         request.catch(function () {
            if (_scriptRegistry.get(key) === request) _scriptRegistry.delete(key);
         });
         return request.then(function () { _removeNode(old); });
      }

      return load().then(function () {
         return true;
      }, function (error) {
         _removeNode(old);
         if (!isCurrent || isCurrent()) _reportScriptError(target, old, error);
         return false;
      });
   }

   function _runScripts(el) {
      if (!el || !el.querySelectorAll) return Promise.resolve();

      var token = (_scriptRunTokens.get(el) || 0) + 1;
      _scriptRunTokens.set(el, token);
      var scripts = Array.prototype.slice.call(el.querySelectorAll('script'));
      var seenExternal = new Set();
      var errors = [];
      var chain = Promise.resolve();

      function isCurrent() { return _scriptRunTokens.get(el) === token; }

      scripts.forEach(function (old) {
         chain = chain.then(function () {
            if (!isCurrent() || !old.parentNode) return;

            var type = _scriptType(old.getAttribute('type'));
            if (!_isExecutableScript(type)) return;

            var src = old.getAttribute('src');
            if (src) {
               var normalized = _scriptURL(src);
               var key = _scriptKey(old, normalized);
               if (seenExternal.has(key)) {
                  _removeNode(old);
                  return;
               }
               seenExternal.add(key);
               return _loadExternalScript(old, el, isCurrent).then(function (loaded) {
                  if (!loaded) errors.push(new Error('Failed to load ' + normalized));
                  if (!isCurrent()) return;
               });
            }

            var spaRun = (old.getAttribute('data-spa-run') || '').toLowerCase().trim();
            if (spaRun !== 'always' && spaRun !== 'once') {
               // The browser already ran inline scripts on the initial full
               // page load. A fragment must opt in before it can execute again.
               _removeNode(old);
               return;
            }

            var content = old.textContent || '';
            var onceKey = old.getAttribute('data-spa-id') || _hashStr(type + '\u0000' + content);
            if (spaRun === 'once' && _onceScripts.has(onceKey)) {
               _removeNode(old);
               return;
            }

            var fresh = _cloneScript(old);
            if (spaRun === 'once') _onceScripts.add(onceKey);

            if (type === 'module') {
               return new Promise(function (resolve, reject) {
                  fresh.onload = resolve;
                  fresh.onerror = function () { reject(new Error('Inline module execution failed')); };
                  old.parentNode.replaceChild(fresh, old);
               }).then(function () {
                  _removeNode(fresh);
               }, function (error) {
                  _removeNode(fresh);
                  if (spaRun === 'once') _onceScripts.delete(onceKey);
                  errors.push(error);
                  _reportScriptError(el, old, error);
               });
            }

            // Classic inline code executes synchronously when the replacement
            // node enters the document; removing it immediately is safe.
            old.parentNode.replaceChild(fresh, old);
            _removeNode(fresh);
         });
      });

      return chain.then(function () {
         if (!isCurrent()) return;
         window.dispatchEvent(new CustomEvent('breeze:scriptsready', {
            detail: { target: el, errors: errors }
         }));
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

   // ── DOM patching ───────────────────────────────────────────────────────
   //
   // swap() used to assign el.innerHTML directly, which destroys and rebuilds
   // every node in the region on every render. That silently wiped anything
   // the DOM itself owned rather than the server: a focused input and its
   // cursor, an open <details>, a canvas someone had drawn into, a
   // third-party widget instance, scroll position inside the region. It also
   // made "this node is new" indistinguishable from "this node's text
   // changed", so there was nowhere honest to hang a component lifecycle.
   //
   // _patch walks the old and new trees together and mutates in place.
   // Matching is by an explicit data-key when present, else tag name plus
   // position. Matched nodes are never replaced — only their changed
   // attributes and text are touched, then we recurse into their children.
   //
   // Escape hatch: a subtree marked data-spa-static is left completely
   // alone, for DOM owned by something other than Breeze (a chart library's
   // own nodes, for example).
   //
   // Fallback: set window.__BREEZE_NO_PATCH__ = true to restore the old
   // innerHTML behaviour if patching ever regresses. The fallback cannot
   // fire onDestroy hooks, because innerHTML discards the old nodes without
   // telling anyone they went away.
   function _parseHTML(html) {
      var tpl = document.createElement('template');
      if (tpl && 'content' in tpl) {
         tpl.innerHTML = html;
         return tpl.content;
      }
      // Engines with no <template>: a detached div parses ordinary fragments
      // well enough (table-row fragments are the known exception).
      var div = document.createElement('div');
      div.innerHTML = html;
      return div;
   }

   function _childArray(node) {
      var out = [], kids = (node && node.childNodes) || [], i;
      for (i = 0; i < kids.length; i++) out.push(kids[i]);
      return out;
   }

   function _attrArray(el) {
      var out = [], attrs = (el && el.attributes) || [], i;
      for (i = 0; i < attrs.length; i++) out.push({ name: attrs[i].name, value: attrs[i].value });
      return out;
   }

   function _isStatic(node) {
      return !!(node && node.nodeType === 1 && node.hasAttribute && node.hasAttribute('data-spa-static'));
   }

   function _nodeKey(node) {
      if (!node || node.nodeType !== 1 || !node.getAttribute) return null;
      return node.getAttribute('data-key');
   }

   function _sameType(a, b) {
      if (!a || !b || a.nodeType !== b.nodeType) return false;
      if (a.nodeType !== 1) return true;
      if ((a.tagName || '') !== (b.tagName || '')) return false;
      var oldID = a.getAttribute && a.getAttribute('id');
      var newID = b.getAttribute && b.getAttribute('id');
      if ((oldID || newID) && oldID !== newID) return false;
      // Static-ness is part of compatibility, not just a skip-flag. If the old
      // node is static and the new one is not, reusing it would strand a node
      // this runtime is forbidden to patch at a position the server now wants
      // to manage — it would keep its stale attributes and children forever.
      // Treating the pair as incompatible replaces it instead.
      return _isStatic(a) === _isStatic(b);
   }

   // Attribute names the last render declared on an element, so a patch can
   // tell "the server removed this attribute" from "your own JS added this
   // attribute". Without the distinction, syncing attributes deletes a class
   // toggled by a click handler, an aria-expanded set by a menu, or a data-*
   // written by a widget — which is exactly the kind of quiet state loss the
   // patcher exists to stop.
   var _ownedAttrs = new WeakMap();

   function _recordOwned(node) {
      if (!node || node.nodeType !== 1) return;
      var names = [], attrs = _attrArray(node), i;
      for (i = 0; i < attrs.length; i++) names.push(attrs[i].name);
      _ownedAttrs.set(node, names);
      // A static subtree is never patched, so walking its descendants only
      // adds startup work and WeakMap entries that can never be consulted.
      if (_isStatic(node)) return;
      var kids = node.childNodes || [];
      for (i = 0; i < kids.length; i++) _recordOwned(kids[i]);
   }

   // The runtime is injected immediately after <body>, before #breeze-app is
   // parsed. Capture the initial server-owned attributes once the document is
   // complete, before users can interact with controls. This one-time pass is
   // important: a lazy first-patch snapshot would mistake client-side state
   // (e.g. a class added by a widget before the first navigation) for a
   // server-owned attribute and remove it when the next fragment omits it.
   function _primeInitialOwnedAttrs() {
      var app = document.getElementById('breeze-app');
      if (app) _recordOwned(app);
   }
   if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', _primeInitialOwnedAttrs, { once: true });
   } else {
      _primeInitialOwnedAttrs();
   }

   // Copy only the attributes that actually differ. Rewriting an attribute
   // that already holds the right value is not free: it can restart a CSS
   // transition or invalidate layout for no reason.
   //
   // Removal is limited to attributes a previous render declared. The initial
   // app subtree is primed once at DOMContentLoaded; nodes introduced later are
   // recorded when inserted, with a lazy fallback for detached patch targets.
   // This preserves first-swap removals without a full-tree scan per navigation.
   function _patchAttrs(oldEl, newEl) {
      var newAttrs = _attrArray(newEl), declared = new Set(), i, a;
      var owned = _ownedAttrs.get(oldEl);
      if (!owned) {
         owned = _attrArray(oldEl).map(function (attr) { return attr.name; });
         _ownedAttrs.set(oldEl, owned);
      }
      for (i = 0; i < newAttrs.length; i++) {
         a = newAttrs[i];
         declared.add(a.name);
         if (oldEl.getAttribute(a.name) !== a.value) oldEl.setAttribute(a.name, a.value);
      }
      for (i = 0; i < owned.length; i++) {
         if (!declared.has(owned[i])) oldEl.removeAttribute(owned[i]);
      }
      _ownedAttrs.set(oldEl, newAttrs.map(function (attr) { return attr.name; }));
   }

   function _patchNode(oldNode, newNode) {
      if (oldNode.nodeType === 3 || oldNode.nodeType === 8) {
         if (oldNode.textContent !== newNode.textContent) oldNode.textContent = newNode.textContent;
         return;
      }
      if (oldNode.nodeType !== 1) return;
      if (_isStatic(oldNode) || _isStatic(newNode)) return;
      _patchAttrs(oldNode, newNode);
      _patchChildren(oldNode, newNode);
   }

   function _patchChildren(oldParent, newParent) {
      var newKids = _childArray(newParent);
      var keyed = null, i;

      function keyedNodes() {
         if (keyed) return keyed;
         keyed = new Map();
         var oldChildren = oldParent.childNodes || [];
         for (var n = 0; n < oldChildren.length; n++) {
            var oldChild = oldChildren[n];
            var oldKey = _nodeKey(oldChild);
            if (oldKey) keyed.set(oldKey, oldChild);
         }
         return keyed;
      }

      var cursor = 0;
      for (i = 0; i < newKids.length; i++) {
         var newNode = newKids[i];
         var key = _nodeKey(newNode);
         // childNodes is live. Indexing it avoids allocating a new array on
         // every iteration while still observing keyed moves/inserts.
         var atCursor = oldParent.childNodes[cursor] || null;
         var match = null;

         if (key) {
            var keyedMatch = keyedNodes().get(key);
            if (keyedMatch && _sameType(keyedMatch, newNode)) {
               match = keyedMatch;
               keyed.delete(key);
            }
         } else if (atCursor && !_nodeKey(atCursor) && _sameType(atCursor, newNode)) {
            match = atCursor;
         }

         if (match) {
            if (match !== atCursor) oldParent.insertBefore(match, atCursor);
            _patchNode(match, newNode);
            _mountSelf(match);
         } else {
            oldParent.insertBefore(newNode, atCursor);
            _recordOwned(newNode);
            _runMounts(newNode);
         }
         cursor++;
      }

      // Remove leftovers from the cursor forward without materialising yet
      // another child array. The live collection shrinks as nodes are removed.
      while ((oldParent.childNodes || []).length > cursor) {
         var leftover = (oldParent.childNodes || [])[cursor];
         _runDestroys(leftover);
         oldParent.removeChild(leftover);
      }
   }

   function _patch(oldEl, newTree) {
      _patchChildren(oldEl, newTree);
   }

   // ── Component lifecycle: onMount / onDestroy ───────────────────────────
   //
   // Breeze.onMount(selector, fn) runs fn(el) exactly once, when el is really
   // inserted into the DOM. fn may return a cleanup function, which runs
   // exactly once when el is really removed. Both are driven from _patch's
   // insert/remove paths, so they do NOT re-fire when a surviving element's
   // content merely updates — that is the whole point of the hook, and the
   // reason it could not exist before patching landed.
   //
   // Declarative equivalent, usable from any template:
   //   <canvas data-spa-mount="initFleetChart"></canvas>
   // calls window.initFleetChart(el) under the same once-per-insert contract.
   //
   // Not to be confused with data-spa-run="once"/"always" (see _runScripts):
   // that governs when an inline <script> in a fragment is re-executed, which
   // is about arbitrary script timing, not an element's lifetime. A
   // data-spa-run="always" script re-runs on every swap; an onMount callback
   // deliberately does not.
   var _mountRules = [];
   var _mountState = new WeakMap(); // el -> { ruleKey: true }
   var _destroyFns = new WeakMap(); // el -> [cleanup, ...]
   var _mountSeq   = 0;

   function _forEachMatch(root, selector, cb) {
      if (!root || root.nodeType !== 1) return;
      if (root.matches && root.matches(selector)) cb(root);
      var found = root.querySelectorAll ? root.querySelectorAll(selector) : [];
      for (var i = 0; i < found.length; i++) cb(found[i]);
   }

   function _mountEl(el, ruleKey, fn) {
      var state = _mountState.get(el);
      if (!state) { state = {}; _mountState.set(el, state); }
      if (state[ruleKey]) return;
      state[ruleKey] = true;

      var cleanup = null;
      try { cleanup = fn(el); } catch (e) { console.error('Breeze.onMount error:', e); }
      if (typeof cleanup === 'function') {
         var list = _destroyFns.get(el) || [];
         list.push(cleanup);
         _destroyFns.set(el, list);
      }
   }

   function _mountRuleOver(root, rule) {
      _forEachMatch(root, rule.selector, function (el) { _mountEl(el, rule.key, rule.fn); });
   }

   // Mount rules against one element only, ignoring its subtree.
   //
   // Note the asymmetry with onDestroy, which fires only on real removal: an
   // element that stops matching a selector (say the render dropped the class)
   // is NOT destroyed, because it is still on the page and its widget may
   // still be driving it. Mount tracks "is this element one of these now",
   // destroy tracks "is this element gone" — conflating them would tear down
   // live widgets on an unrelated class change.
   function _mountSelf(el) {
      if (!el || el.nodeType !== 1) return;
      var i;
      for (i = 0; i < _mountRules.length; i++) {
         if (el.matches && el.matches(_mountRules[i].selector)) {
            _mountEl(el, _mountRules[i].key, _mountRules[i].fn);
         }
      }
      if (el.hasAttribute && el.hasAttribute('data-spa-mount')) {
         var name = el.getAttribute('data-spa-mount');
         if (name && typeof window[name] === 'function') _mountEl(el, 'attr:' + name, window[name]);
      }
   }

   function _runMounts(root) {
      if (!root || root.nodeType !== 1) return;
      for (var i = 0; i < _mountRules.length; i++) _mountRuleOver(root, _mountRules[i]);
      _forEachMatch(root, '[data-spa-mount]', function (el) {
         var name = el.getAttribute('data-spa-mount');
         if (!name) return;
         var fn = window[name];
         if (typeof fn === 'function') _mountEl(el, 'attr:' + name, fn);
         else console.warn('data-spa-mount: window.' + name + ' is not a function');
      });
   }

   function _destroyOne(el) {
      var list = _destroyFns.get(el);
      _mountState.delete(el);
      if (!list) return;
      _destroyFns.delete(el);
      for (var i = 0; i < list.length; i++) {
         try { list[i](); } catch (e) { console.error('Breeze onDestroy error:', e); }
      }
   }

   function _runDestroys(root) {
      if (!root || root.nodeType !== 1) return;
      var all = root.querySelectorAll ? root.querySelectorAll('*') : [];
      for (var i = 0; i < all.length; i++) _destroyOne(all[i]);
      _destroyOne(root);
   }

   function _registerMount(selector, fn) {
      if (typeof selector !== 'string' || typeof fn !== 'function') {
         console.warn('Breeze.onMount(selector, fn) requires a selector string and a function');
         return function () {};
      }
      var rule = { selector: selector, fn: fn, key: 'rule:' + (_mountSeq++) };
      _mountRules.push(rule);

      // Mount anything already on screen, so registering after the first
      // render is not a silent no-op.
      var root = document.getElementById('breeze-app') || document.body;
      if (root) _mountRuleOver(root, rule);

      return function unregister() {
         var i = _mountRules.indexOf(rule);
         if (i !== -1) _mountRules.splice(i, 1);
      };
   }

   // Prepare route state before patching so every onMount/data-spa-mount hook
   // sees the new route's data synchronously. Watchers and bindings are notified
   // only after the DOM patch has completed, avoiding a render against old DOM.
   function swap(el, html) {
      if (!el) return Promise.resolve();
      var newTree = _parseHTML(html);
      var stateChange = _prepareStateTags(newTree);

      if (window.__BREEZE_NO_PATCH__ === true) {
         _runDestroys(el);
         while (el.firstChild) el.removeChild(el.firstChild);
         el.appendChild(newTree);
         _recordOwned(el);
         _runMounts(el);
      } else {
         _patch(el, newTree);
      }

      _finishStateTags(stateChange);
      return _runScripts(el);
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
         signal: options.signal,
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

         if (el) { await swap(el, html); }
         if (options.onSuccess) { options.onSuccess(html, el); }

         window.dispatchEvent(new CustomEvent('breeze:update', {
         detail: { url: url, target: target, html: html }
         }));

         return html;
      } catch (e) {
         // Cancellation is a lifecycle decision, not an application failure.
         // In particular, a stopped poll or superseded page request must not
         // invoke a caller's error UI (toast, retry prompt, etc.).
         if (options.onError && (!e || e.name !== 'AbortError')) { options.onError(e, el); }
         throw e;
      }
   }

   // ── Polling ────────────────────────────────────────────────────────────

   var _pollsByNode = new WeakMap();
   var _pollsByKey = new Map();

   function _pollKey(target, el) {
      if (typeof target === 'string') return 'selector:' + target;
      if (target == null) return 'default';
      var locator = _bindLocator(el || target);
      return locator ? 'locator:' + locator : null;
   }

   function _stopPollRecord(record) {
      if (!record || record.stopped) return;
      record.stopped = true;
      if (record.timer !== null) clearTimeout(record.timer);
      record.timer = null;
      if (record.controller) record.controller.abort();
      record.controller = null;
      if (record.externalSignal && record.externalAbort) {
         record.externalSignal.removeEventListener('abort', record.externalAbort);
      }
      record.externalAbort = null;
      if (record.key && _pollsByKey.get(record.key) === record) _pollsByKey.delete(record.key);
   }

   function breezePoll(url, target, intervalMs, options) {
      var el = resolveEl(target);
      if (!el) { console.warn('breeze.poll: target not found', target); return; }

      var key = _pollKey(target, el);
      if (key && _pollsByKey.has(key)) _stopPollRecord(_pollsByKey.get(key));
      var site = _makeSite((target && typeof target !== 'string') ? el : target);
      var externalSignal = options && options.signal;
      var record = {
         key: key,
         site: site,
         timer: null,
         controller: null,
         stopped: false,
         interval: Math.max(1, Number(intervalMs) || 5000),
         current: el,
         externalSignal: externalSignal || null,
         externalAbort: null,
      };
      if (key) _pollsByKey.set(key, record);
      _pollsByNode.set(el, record);

      if (externalSignal) {
         if (externalSignal.aborted) {
            _stopPollRecord(record);
            return function stop() { _stopPollRecord(record); };
         }
         record.externalAbort = function () { _stopPollRecord(record); };
         externalSignal.addEventListener('abort', record.externalAbort, { once: true });
      }

      function schedule() {
         if (record.stopped) return;
         record.timer = setTimeout(tick, record.interval);
      }

      function tick() {
         if (record.stopped) return;
         var live = _bindTarget(record.site);
         if (live === null) { _stopPollRecord(record); return; }
         record.current = live;
         _pollsByNode.set(live, record);

         var controller = new AbortController();
         record.controller = controller;
         var requestOptions = Object.assign({}, options || {}, { signal: controller.signal });
         breezeGet(url, live, requestOptions).catch(function (error) {
            if (!error || error.name !== 'AbortError') return;
         }).finally(function () {
            if (record.controller === controller) record.controller = null;
            // Fixed-delay scheduling: at most one request is in flight, and
            // time spent fetching is not followed by a burst of overdue ticks.
            if (!record.stopped) schedule();
         });
      }

      tick();
      return function stop() { _stopPollRecord(record); };
   }

   function breezeStop(target) {
      var record = null;
      if (typeof target === 'string') {
         record = _pollsByKey.get('selector:' + target) ||
                  _pollsByKey.get('locator:' + target) || null;
         if (!record) {
            var found = document.querySelector(target);
            if (found) record = _pollsByNode.get(found) || null;
         }
      } else if (target == null) {
         record = _pollsByKey.get('default') || null;
      } else {
         record = _pollsByNode.get(target) || null;
         if (!record) {
            var key = _pollKey(target, target);
            if (key) record = _pollsByKey.get(key) || null;
         }
      }
      if (record) _stopPollRecord(record);
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

   // During the async part of a navigation the address bar is still on the
   // previous route, but mounts/scripts already belong to the incoming one.
   // Keep its cache key scoped to that swap so a data mutation in onMount or
   // an awaited route script invalidates the fragment currently being shown.
   var _pendingRouteCache = null;
   function _currentRouteCacheKey() {
      return _pendingRouteCache ? _pendingRouteCache.key :
         _normalizeUrl(window.location.pathname + window.location.search);
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
         var pendingRoute = { key: key };
         _pendingRouteCache = pendingRoute;
         try {
            await swap(target, html);
         } finally {
            if (_pendingRouteCache === pendingRoute) _pendingRouteCache = null;
         }
         if (seq !== _navSeq) return;

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
   //   - a page handler already called preventDefault() (e.g. to submit
   //     via fetch itself)
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
      if (e.defaultPrevented) return;
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
         return res.text().then(async function (html) {
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
         var pendingRoute = { key: _normalizeUrl(actionPath) };
         _pendingRouteCache = pendingRoute;
         try {
            await swap(target, html);
            if (_isCacheable(res)) _cacheRoute(pendingRoute.key, html);
            history.pushState({ breezeUrl: actionHref, scrollY: 0 }, '', actionHref);
            window.scrollTo(0, 0);
            window.dispatchEvent(new CustomEvent('breeze:navigate', { detail: { url: actionPath } }));
            _runHooks('afterSubmit', { form: form, url: actionPath, html: html });
         } finally {
            if (_pendingRouteCache === pendingRoute) _pendingRouteCache = null;
         }
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
   var _bindings    = []; // { name, key, el, locator } registered via breeze.bind()
   var _renderTimer = null;

   // ── Where a re-render renders TO ────────────────────────────────────────
   //
   // A binding records how to FIND its region, not which node object it found
   // last time. That distinction is the whole reactivity bug.
   //
   // bind() used to store the resolved element. The patcher is free to replace
   // a node whose type no longer matches, or to remove it as leftover, and
   // when it did, the binding kept pointing at the detached original. Every
   // later store mutation still ran the full path — proxy trap, watchers,
   // template render, swap, _patch — and patched that orphan correctly. The
   // document never changed. The update only appeared once a navigation
   // re-ran the view's inline script and re-registered the binding against
   // the new node, which is precisely why the symptom read as "data only
   // updates on tab change, not reactively".
   //
   // Navigation and poll/fetch never hit this because they resolve their
   // target through resolveEl at render time (usually #breeze-app, which is
   // never replaced). Store-driven renders were the one path rendering into a
   // remembered node, so they were the one path that could go stale.
   //
   // The locator uses the same identity discipline as _patch's own node
   // matching: an explicit key if the element has one (id, then data-key),
   // otherwise a generated data-spa-bind stamp so the replacement node can
   // still be found. String and null targets were always re-resolved by
   // resolveEl and are left exactly as they were.
   var _bindSeq = 0;

   function _isLive(node) {
      // Walked rather than asking isConnected: that property is missing in
      // older engines and in minimal DOM implementations, where reading it
      // yields undefined and would condemn every node as detached.
      for (var n = node; n; n = n.parentNode) {
         if (n === document.body || n === document.documentElement || n.nodeType === 9) return true;
      }
      return false;
   }

   // A site is only ever declared dead on positive evidence: the node was
   // reachable from the document when it was registered, and is not now.
   //
   // Without that "was", anything legitimately outside the document tree —
   // an element bound before it is inserted, a container the developer
   // attaches later, a host environment whose nodes never report a path to
   // document.body — would read as removed on its very first update and stop
   // rendering. That is a worse failure than the stale-node bug this is
   // fixing, so absence of evidence is treated as "still fine, render into
   // it", exactly as the runtime behaved before.
   function _makeSite(target) {
      var isEl = !!(target && typeof target !== 'string' && target.nodeType === 1);
      if (!isEl) return { el: target, locator: null, wasLive: false };
      return { el: target, locator: _bindLocator(target), wasLive: _isLive(target) };
   }

   function _bindLocator(el) {
      if (!el || el.nodeType !== 1) return null;
      var id = el.getAttribute && el.getAttribute('id');
      if (id) return '#' + id;
      var key = _nodeKey(el);
      if (key) return '[data-key="' + key + '"]';
      if (!el.setAttribute) return null;
      var stamp = el.getAttribute('data-spa-bind');
      if (!stamp) {
         stamp = 'b' + (++_bindSeq);
         el.setAttribute('data-spa-bind', stamp);
      }
      return '[data-spa-bind="' + stamp + '"]';
   }

   // The element this site should render into right now, or null if its region
   // has genuinely left the document.
   function _bindTarget(site) {
      if (!site.el || typeof site.el === 'string') return resolveEl(site.el);
      if (_isLive(site.el)) return site.el;

      // Detached. If a replacement is findable, adopt it: from here on this
      // site tracks the live node.
      if (site.locator) {
         var fresh = document.querySelector(site.locator);
         if (fresh && fresh !== site.el) {
            site.el = fresh;
            site.wasLive = true;
            return fresh;
         }
      }
      // Genuinely removed, and it was on the page before — stop.
      if (site.wasLive) return null;
      // Never was reachable; not this code's business to declare it gone.
      return site.el;
   }

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
         _invalidateRoute(_currentRouteCacheKey());
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
      // Iterate a snapshot: a rendered fragment's inline script may call
      // bind() or unbind() while this pass is still running, and mutating the
      // list underneath the loop would skip or repeat entries.
      var snapshot = _bindings.slice();
      var dead = null;
      try {
         for (var i = 0; i < snapshot.length; i++) {
            var b  = snapshot[i];
            var el = _bindTarget(b);
            if (el === null) { (dead || (dead = [])).push(b); continue; }
            var data = b.key ? (_store ? _store[b.key] : undefined) : _store;
            _rerender(b.name, data, el);
         }
      } finally {
         _renderBatch = outer;
         // Drop bindings whose region left the document, rather than retrying
         // them on every mutation forever. Filtering the CURRENT list (not the
         // snapshot) keeps anything registered mid-pass.
         if (dead) {
            _bindings = _bindings.filter(function (x) { return dead.indexOf(x) === -1; });
         }
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

   // State tags are extracted from the detached fragment before any new nodes
   // are mounted. This makes breeze.data() route-correct inside onMount without
   // setTimeout. Cache invalidation is selective; an ordinary fragment update
   // does not discard unrelated template/i18n caches.
   function _prepareStateTags(newTree) {
      var state = { data: false, i18n: false, tmpl: false };
      [
         ['__breeze_data__', 'data'],
         ['__breeze_i18n__', 'i18n'],
         ['__breeze_tmpl__', 'tmpl'],
      ].forEach(function (entry) {
         var fresh = newTree.querySelector('#' + entry[0]);
         if (!fresh) return;
         var existing = document.getElementById(entry[0]);
         if (existing && existing !== fresh && existing.parentNode) {
            existing.parentNode.removeChild(existing);
         }
         document.body.appendChild(fresh);
         state[entry[1]] = true;
      });

      if (state.i18n) _i18nCache = null;
      if (state.tmpl) _tmplCache = null;
      if (state.data && _storeRenderDepth === 0) {
         // A mutation of the previous route may already have queued a
         // debounced watcher/render pass. The new route snapshot supersedes
         // that pending pass; let _finishStateTags deliver one notification
         // against the new DOM instead of notifying twice with mixed routes.
         if (_renderTimer !== null) {
            clearTimeout(_renderTimer);
            _renderTimer = null;
         }
         // Hydrate synchronously before _patchChildren can run any mount hook.
         _store = _makeReactive(_readDataTag(), _onStoreChange);
      }
      return state;
   }

   function _finishStateTags(state) {
      if (state.data && _storeRenderDepth === 0) {
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

   // Depth of swaps caused by the store rather than by the server. Read by
   // _prepareStateTags to decide whether a fragment's data tag is newer than
   // the store or older than it. Incremented around each swap rather than
   // around the whole render so it is still accurate for the server path,
   // whose swap happens later, inside a promise callback.
   var _storeRenderDepth = 0;

   function _swapFromStore(el, html) {
      _storeRenderDepth++;
      try { return swap(el, html); } finally { _storeRenderDepth--; }
   }

   function _rerender(name, data, target) {
      var el      = resolveEl(target);
      var sources = _tmplSources();

      if (sources[name] !== undefined) {
         var html = _evalTmpl(sources[name], data, sources, 0);
         var swapped = el ? _swapFromStore(el, html) : Promise.resolve();
         // Preserve the historical event contract: the DOM patch is
         // synchronous, so breeze:render fires immediately after it. Script
         // readiness has its own breeze:scriptsready event and Promise.
         window.dispatchEvent(new CustomEvent('breeze:render', {
            detail: { name: name, target: target, html: html, local: true }
         }));
         return swapped.then(function () { return html; });
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
         var swapped = el ? _swapFromStore(el, out.html) : Promise.resolve();
         window.dispatchEvent(new CustomEvent('breeze:render', {
            detail: { name: name, target: target, html: out.html, local: false }
         }));
         return swapped.then(function () { return out.html; });
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

      // Component lifecycle. fn(el) runs once when a matching element is
      // really inserted into the DOM; the function it optionally returns runs
      // once when that element is really removed. Content updates to an
      // element that stays put do not re-fire either one.
      //
      //   Breeze.onMount('.chart', function (el) {
      //     var chart = initChart(el);
      //     return function () { chart.dispose(); };
      //   });
      //
      // Returns an unregister function. See also data-spa-mount="fnName" for
      // the declarative form.
      onMount: _registerMount,

      // Registers a cleanup for one specific element that has already been
      // mounted, for cases where the element is obtained directly rather than
      // by selector. Runs once, when that element is removed by a patch.
      onDestroy: function(el, fn) {
         if (!el || el.nodeType !== 1 || typeof fn !== 'function') {
            console.warn('Breeze.onDestroy(el, fn) requires an element and a function');
            return;
         }
         var list = _destroyFns.get(el) || [];
         list.push(fn);
         _destroyFns.set(el, list);
      },
   };

   window.breeze = {
      fetch:    breezeGet,
      poll:     breezePoll,
      stop:     breezeStop,
      swap:     function(target, html) { return swap(resolveEl(target), html); },
      navigate: function(url) { return navigate(url, true); },

      data: function(key) {
         if (_store === null) _store = _makeReactive(_readDataTag(), _onStoreChange);
         return key ? (_store && typeof _store === 'object' ? _store[key] : undefined) : _store;
      },

      setData: function(newData, target, name) {
         _store = _makeReactive(newData, _onStoreChange);
         _invalidateRoute(_currentRouteCacheKey());
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
         var el = resolveEl(target);
         // An element target is tracked by identity plus a locator to recover
         // from replacement; a string or null target IS a locator already, and
         // resolveEl re-runs it on every render.
         var b  = _makeSite((target && typeof target !== 'string') ? el : target);
         b.name = name;
         b.key  = key;
         // Re-binding the same region to the same template replaces the
         // previous entry instead of adding a second one. A view's inline
         // script runs again on every swap of that view, so without this the
         // same region accumulated one duplicate binding per render and every
         // mutation re-rendered it N times.
         _bindings = _bindings.filter(function (x) {
            if (x.name !== name || x.key !== key) return true;
            return b.locator !== null ? x.locator !== b.locator : x.el !== b.el;
         });
         _bindings.push(b);
         var data = key ? window.breeze.data(key) : window.breeze.data();
         _rerender(name, data, el);
         return function unbind() { _bindings = _bindings.filter(function(x) { return x !== b; }); };
      },

      // Force the next visit to 'url' (default: the current route) to
      // re-fetch instead of reusing a cached fragment. invalidateAll() clears
      // every cached route. Called automatically on any store mutation and
      // on non-GET SPA form submissions; exposed here for manual use when a
      // mutation happens through some path the framework can't see (e.g. a
      // fetch() call you make yourself).
      invalidate:    function(url) { _invalidateRoute(url || _currentRouteCacheKey()); },
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
