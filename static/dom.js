// Writing to the DOM without throwing the page away.
//
// Every page here refreshes on a timer — once a second on Overview, every five
// seconds on Claude, every fifteen on Deploy — but almost nothing on screen
// actually changed between two frames. Rebuilding a region from an HTML string
// replaces all of it regardless, which drops any text the reader had selected,
// collapses anything they had expanded, resets a scrolled table, and restarts
// every CSS transition. These helpers write only on a real change, and leave an
// unchanged node completely alone.
//
// Loaded once from <head>. The client-side navigation in spa.js re-runs a page's
// inline scripts but not its external ones, so this stays loaded across tabs.

// Assigning textContent replaces the text node even when the string is
// identical, and assigning an attribute rewrites it even when the value is —
// both are real writes, so all of these are guarded.
function setText(el, text) { if (el.textContent !== text) el.textContent = text; }
function setClass(el, cls) { if (el.className !== cls) el.className = cls; }
function setHidden(el, hidden) { if (el.hidden !== hidden) el.hidden = hidden; }
function setStyle(el, prop, val) { if (el.style[prop] !== val) el.style[prop] = val; }
function setAttr(el, name, val) {
  if (val === null || val === undefined || val === false) { if (el.hasAttribute(name)) el.removeAttribute(name); return; }
  if (el.getAttribute(name) !== String(val)) el.setAttribute(name, val);
}
function setProp(el, prop, val) { if (el[prop] !== val) el[prop] = val; }
function setHTML(el, html) {
  if (el._html === html) return;   // cached: reading innerHTML back would serialize the DOM
  el._html = html;
  el.innerHTML = html;
}
// Never fight the person typing: a value only lands in a field they are not in.
function setValue(el, val) {
  if (document.activeElement === el) return;
  if (el.value !== val) el.value = val;
}
function h(tag, cls) { const e = document.createElement(tag); if (cls) e.className = cls; return e; }

/* syncList is the reason a reading can be held on screen. setHTML still rewrites
   a whole region whenever any part of it changed, and a region that carries a
   figure ticking every second changed every second — so the nodes around it were
   thrown away and rebuilt too, taking any selection with them. This keeps one
   element per key across frames, creates and removes only what actually appeared
   or went away, and leaves everything else where it is; the update callback then
   writes individual fields through the guarded setters above, so a number that
   did not move is never touched at all.

   `items` are {key, …} objects. create(item) builds the element; update(el, item)
   fills it in. Keys must be unique within the host.                            */
function syncList(host, items, create, update) {
  const kept = host._keyed || new Map();
  const next = new Map();
  let prev = null;
  for (const it of items) {
    let el = kept.get(it.key);
    if (el) kept.delete(it.key);
    else el = create(it);
    update(el, it);
    // Move it only if it is not already sitting where it belongs.
    const want = prev ? prev.nextSibling : host.firstChild;
    if (el !== want) host.insertBefore(el, want);
    prev = el;
    next.set(it.key, el);
  }
  for (const el of kept.values()) el.remove();
  host._keyed = next;
  host._html = null;   // this host is no longer setHTML's to cache
}

/* The fixed scaffolding of a page — headings, table headers, the empty cells a
   value will later be written into — is the same on every refresh, so it is
   built once and then only filled in. Returns true the first time, for callers
   that need to bind listeners to what was just created. */
function scaffold(host, html) {
  if (host._scaffold === html) return false;
  host._scaffold = html;
  host._keyed = null;
  host.innerHTML = html;
  return true;
}

// What each service is called on screen. The internal names (unit, repo, deploy
// config) stay as they are — they are what systemd, GitHub and the deployer key
// on — but people read "Orders", not "orders-app". Anything not listed here is
// shown as-is, so a new service still appears, just under its raw name until it
// is added.
const SERVICE_LABELS = {
  'vps-health': 'Health', 'orders-app': 'Orders', 'home-app': 'Home', 'media-app': 'Media',
  'transport-service': 'Transport', 'session-controller': 'Sessions', 'mail': 'Mail',
  'milestones': 'Milestones', 'cart': 'Cart', 'osmosis-auth': 'Login', 'osmosis-index': 'Launcher',
  'roofz-crawler': 'Roofz', 'ikwilhuren-crawler': 'Ikwilhuren', 'vesteda-crawler': 'Vesteda',
  'rebo-crawler': 'Rebo', 'nederwoon-crawler': 'NederWoon', 'vbt-crawler': 'vb&t',
  'holland2stay-crawler': 'Holland2Stay',
  'caddy': 'Caddy', 'coder': 'Coder', 'coder-workspace': 'Coder workspace',
  'coder-favicon-proxy': 'Coder icon proxy', 'build-watchdog': 'Build watchdog',
  'hetzner-deals': 'Hetzner deals',
};
function serviceLabel(name) { return SERVICE_LABELS[name] || name; }

// An app's logo, from /icons/<name>.svg. Hidden until it loads, and stays hidden
// for a service that has no icon, so a missing file never shows a broken image.
// Icons are cached for a day, by the browser and by Cloudflare, so a new or
// redrawn icon would not show until that ran out. The server tags this script's
// own address with the build (?v=…); the same tag on every icon address makes each
// deploy fetch them fresh.
const ICON_V = (() => {
  try { return new URL(document.currentScript.src).searchParams.get('v') || ''; } catch (e) { return ''; }
})();

function setIcon(img, name) {
  const src = `/icons/${name}.svg` + (ICON_V ? `?v=${ICON_V}` : '');
  if (img.getAttribute('src') === src) return;   // already right; don't re-fetch
  img.hidden = true;
  img.onload = () => { img.hidden = false; };
  img.onerror = () => { img.hidden = true; };
  img.setAttribute('src', src);
}
