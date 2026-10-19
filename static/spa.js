// Client-side navigation ("SPA boost").
//
// The dashboard is five standalone pages that share one shell. Following a nav
// link normally reloads the whole document — re-parsing all the CSS/JS, tearing
// down and rebuilding the live SSE stream, a white flash. This fetches the target
// page and swaps it in place instead, so switching tabs is instant and the shell
// stays put.
//
// Two rules keep it safe:
//   1. Anything a page starts (timers, the SSE stream, window listeners) is
//      tracked and torn down before the next page loads, so nothing leaks or
//      double-fires. This script runs first (blocking, in <head>), so it even
//      catches what the very first page sets up.
//   2. Any failure — a fetch error, a session-expiry redirect to the login host,
//      an exception — falls back to a normal navigation. A bug here can slow a
//      click down to a full reload, but it can never break a link.
(() => {
  if (window.__spaBoost) return;
  window.__spaBoost = true;

  // Reserve the scrollbar gutter always, so navigating between a page that scrolls
  // and one that doesn't (QA is short, the others are tall) can't toggle the
  // scrollbar — which would otherwise shift the whole centered layout, and the
  // full-bleed header's 50vw math, sideways on every such navigation. Runs in
  // <head> before first paint, and the <html> element survives every swap.
  try { document.documentElement.style.scrollbarGutter = 'stable'; } catch (_) {}

  // ---- track what each page starts, to tear it down on the way out ----
  const timers = [], sources = [], listeners = [];
  const _setInterval = window.setInterval.bind(window);
  const _setTimeout = window.setTimeout.bind(window);
  const _clearInterval = window.clearInterval.bind(window);
  const _clearTimeout = window.clearTimeout.bind(window);
  const NativeES = window.EventSource;
  const _winAdd = window.addEventListener.bind(window);
  const _winRemove = window.removeEventListener.bind(window);
  const _docAdd = document.addEventListener.bind(document);
  const _docRemove = document.removeEventListener.bind(document);

  window.setInterval = (...a) => { const id = _setInterval(...a); timers.push(['i', id]); return id; };
  window.setTimeout = (...a) => { const id = _setTimeout(...a); timers.push(['t', id]); return id; };
  if (NativeES) {
    window.EventSource = function (url, cfg) { const es = new NativeES(url, cfg); sources.push(es); return es; };
    window.EventSource.prototype = NativeES.prototype;
    for (const k of ['CONNECTING', 'OPEN', 'CLOSED']) window.EventSource[k] = NativeES[k];
  }
  window.addEventListener = (type, fn, opts) => { _winAdd(type, fn, opts); listeners.push([window, type, fn, opts]); };
  document.addEventListener = (type, fn, opts) => { _docAdd(type, fn, opts); listeners.push([document, type, fn, opts]); };

  function teardown() {
    for (const [k, id] of timers) (k === 'i' ? _clearInterval : _clearTimeout)(id);
    timers.length = 0;
    for (const es of sources) { try { es.close(); } catch (_) {} }
    sources.length = 0;
    for (const [t, type, fn, opts] of listeners) {
      try { (t === window ? _winRemove : _docRemove)(type, fn, opts); } catch (_) {}
    }
    listeners.length = 0;
  }

  // Scripts parsed by DOMParser / adopted from another document do not execute, so
  // re-run each inline one in a fresh function scope — its top-level const/let stay
  // local, so re-visiting a page never collides with the last visit's globals.
  function runScripts(root) {
    for (const old of root.querySelectorAll('script')) {
      if (old.src) continue;
      const s = document.createElement('script');
      s.textContent = '(function(){\n' + old.textContent + '\n})();';
      document.body.appendChild(s);
      s.remove();
    }
  }

  // Switching tabs replaces the current history entry rather than adding one,
  // so Back leaves the dashboard instead of stepping through every tab visited.
  // The fallbacks below replace too, for the same reason.
  let navToken = 0;
  async function navigate(url, fromClick) {
    const token = ++navToken;
    let doc;
    try {
      const res = await fetch(url, { credentials: 'same-origin' });
      // A session-expiry redirect lands on the login host (cross-origin); let the
      // browser handle that as a real navigation.
      if (res.redirected && new URL(res.url).origin !== location.origin) throw 0;
      if (!res.ok || !(res.headers.get('content-type') || '').includes('text/html')) throw 0;
      doc = new DOMParser().parseFromString(await res.text(), 'text/html');
    } catch (_) {
      location.replace(url);
      return;
    }
    if (token !== navToken) return; // superseded by a newer click

    const swap = () => {
      try {
        teardown();
        document.title = doc.title;
        // Swap the page-specific <style> blocks; the icon <link>s and this script stay.
        document.head.querySelectorAll('style').forEach((el) => el.remove());
        doc.head.querySelectorAll('style').forEach((el) => document.head.appendChild(document.importNode(el, true)));
        // Swap the whole body: its header already carries the right current tab and
        // the page's own extras (the live pill on Overview, Sign out on Deploy).
        document.body.replaceWith(document.adoptNode(doc.body));
        runScripts(document.body);
        window.scrollTo(0, 0);
      } catch (e) {
        console.error('spa: swap failed', e);
        location.replace(url);
      }
    };

    // No whole-page cross-fade: every page shares the same shell, so fading one
    // into the next just double-exposes two near-identical pages, a visible
    // flicker. The page swaps in one paint. The swap does run inside a view
    // transition, but the pages leave their root out of it (see "Tab pill" in
    // their CSS), so the only things that move are the tab pill sliding to the
    // new tab and the tab labels changing colour.
    if (fromClick) history.replaceState({ spa: true }, '', url);
    if (document.startViewTransition && document.querySelector('.navpill') &&
        !matchMedia('(prefers-reduced-motion: reduce)').matches) {
      try { document.startViewTransition(swap); return; } catch (_) {}
    }
    swap();
  }

  _docAdd('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target.closest('a');
    if (!a || a.target === '_blank' || a.hasAttribute('download')) return;
    const href = a.getAttribute('href');
    if (!href || href.startsWith('#')) return;
    let url;
    try { url = new URL(a.href, location.href); } catch (_) { return; }
    if (url.origin !== location.origin || url.pathname === '/logout') return; // external / sign-out → real nav
    e.preventDefault();
    if (url.href !== location.href) navigate(url.pathname + url.search, true);
  });

  _winAdd('popstate', () => navigate(location.pathname + location.search, false));
})();
