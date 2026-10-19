
// iOS Safari only applies :active (the pressed look) once some touch listener exists.
document.addEventListener('touchstart', function () {}, { passive: true });
// The tab pill: the green pill behind the current tab.
//
// It sits under the current tab, and it can be dragged: put a finger (or the
// mouse) on the tabs and slide, and the pill follows along, lighting up the tab
// it is over; let go on another tab and that tab opens, the pill carrying on
// from wherever it was left. A plain tap still works exactly as before. With a
// mouse, the pill also slides to whichever tab is hovered, and back to the
// current tab once the mouse leaves the tabs.
//
// Loaded once from <head>, like dom.js. spa.js swaps the whole body on every
// tab switch and re-runs each page's inline scripts, and each page's inline
// call to navPill() wires up its fresh header. Everything here hangs off the
// nav element itself, so it all goes away with the old body.
function navPill() {
  const nav = document.querySelector('header.console .nav');
  const pill = nav && nav.querySelector('.navpill');
  const cur = nav && nav.querySelector('.navlink.current');
  if (!pill || !cur) return;
  const tabs = [...nav.querySelectorAll('.navlink')].filter(a => !a.classList.contains('signout'));

  const put = (a) => {
    pill.style.left = a.offsetLeft + 'px';
    pill.style.top = a.offsetTop + 'px';
    pill.style.width = a.offsetWidth + 'px';
    pill.style.height = a.offsetHeight + 'px';
  };
  let drag = null;
  let steering = false; // a drag or hover is in progress or still settling
  let hovering = null;   // the tab under the mouse, while it is over one
  let rest = 0;
  const place = () => { if (!steering) put(cur); };
  place();
  nav.classList.add('pilled');
  if (window.ResizeObserver) {
    const ro = new ResizeObserver(place);
    ro.observe(nav); tabs.forEach(a => ro.observe(a));
  }

  // A link dragged with the mouse would otherwise start the browser's own
  // drag-and-drop and steal the gesture.
  tabs.forEach(a => { a.draggable = false; });

  const nearest = (x) => {
    let best = cur, bestD = Infinity;
    for (const a of tabs) {
      const r = a.getBoundingClientRect();
      const d = Math.abs(x - (r.left + r.width / 2));
      if (d < bestD) { bestD = d; best = a; }
    }
    return best;
  };
  const light = (a) => {
    tabs.forEach(t => t.classList.toggle('over', t === a));
  };
  // Slides the pill to a tab other than the current one, lit up.
  const visit = (a) => {
    clearTimeout(rest);
    steering = true;
    nav.classList.add('steer');
    light(a); put(a);
  };
  // Slides the pill back under the current tab, then drops the steering look
  // once it has landed.
  const home = () => {
    light(cur); put(cur);
    clearTimeout(rest);
    rest = setTimeout(() => {
      if (drag || hovering) return;
      steering = false;
      nav.classList.remove('steer');
      tabs.forEach(t => t.classList.remove('over'));
    }, 350);
  };

  nav.addEventListener('pointerover', (e) => {
    if (e.pointerType !== 'mouse' || drag) return;
    const a = e.target.closest('.navlink');
    if (!a) return; // the gap between tabs: stay where it is
    if (!tabs.includes(a)) { hovering = null; home(); return; } // sign out
    if (a !== hovering) { hovering = a; visit(a); }
  });
  nav.addEventListener('pointerleave', (e) => {
    if (e.pointerType !== 'mouse' || !hovering) return;
    hovering = null;
    if (!drag) home();
  });

  nav.addEventListener('pointerdown', (e) => {
    if (e.button !== 0 || !e.isPrimary) return;
    drag = { id: e.pointerId, x0: e.clientX, moved: false, over: cur };
  });

  nav.addEventListener('pointermove', (e) => {
    if (!drag || e.pointerId !== drag.id) return;
    if (!drag.moved) {
      // A small wobble is still a tap.
      if (Math.abs(e.clientX - drag.x0) < 8) return;
      drag.moved = steering = true;
      nav.classList.add('steer', 'dragging');
      try { nav.setPointerCapture(e.pointerId); } catch (_) {}
    }
    const over = nearest(e.clientX);
    const w = over.offsetWidth;
    const first = tabs[0], last = tabs[tabs.length - 1];
    const min = first.offsetLeft, max = last.offsetLeft + last.offsetWidth - w;
    const x = e.clientX - nav.getBoundingClientRect().left - w / 2;
    pill.style.left = Math.max(min, Math.min(max, x)) + 'px';
    pill.style.width = w + 'px';
    if (over !== drag.over) { drag.over = over; light(over); }
  });

  // Swallows the click the browser sends after a drag, so a drag that ends on
  // a link does not open it a second time on top of the navigation below.
  let swallow = false;
  nav.addEventListener('click', (e) => {
    if (swallow) { e.preventDefault(); e.stopPropagation(); }
  }, true);

  const end = (e, commit) => {
    if (!drag || e.pointerId !== drag.id) return;
    const d = drag;
    drag = null;
    if (!d.moved) return; // a plain tap: the click does the rest
    nav.classList.remove('dragging');
    const target = commit ? d.over : cur;
    // Settles onto the tab, through the .steer transition.
    if (target === cur) home(); else visit(target);
    swallow = true;
    setTimeout(() => {
      swallow = false;
      if (target !== cur) target.click(); // spa.js takes it from here, pill and all
    }, 0);
  };
  nav.addEventListener('pointerup', (e) => end(e, true));
  nav.addEventListener('pointercancel', (e) => end(e, false));

  // Swiping the page left or right moves to the next or previous tab, like
  // flicking between pages in an app. Only a clear sideways flick counts: it must
  // travel 60px, mostly sideways, within 600ms. Left alone: swipes that start on
  // the tabs themselves (the pill drag above), in a form field, in anything that
  // scrolls sideways (the Claude tab's wide table), anything marked data-noswipe
  // (the Hetzner chart, which a sideways drag turns), or within 24px of the screen
  // edge, where the browser's own back/forward swipe lives. Registered through
  // document.addEventListener, so spa.js removes it with the page it belongs to.
  const scrollsSideways = (el) => {
    for (; el && el !== document.body; el = el.parentElement) {
      const ox = getComputedStyle(el).overflowX;
      if ((ox === 'auto' || ox === 'scroll') && el.scrollWidth > el.clientWidth + 1) return true;
    }
    return false;
  };
  let swipe = null;
  document.addEventListener('touchstart', (e) => {
    swipe = null;
    if (e.touches.length !== 1) return;
    const t = e.touches[0];
    if (t.clientX < 24 || t.clientX > window.innerWidth - 24) return;
    if (e.target.closest('header.console .nav, input, textarea, select, [contenteditable], [data-noswipe]')) return;
    if (scrollsSideways(e.target)) return;
    swipe = { x: t.clientX, y: t.clientY, at: Date.now() };
  }, { passive: true });
  document.addEventListener('touchcancel', () => { swipe = null; }, { passive: true });
  document.addEventListener('touchend', (e) => {
    const s = swipe;
    swipe = null;
    if (!s || e.touches.length) return;
    const t = e.changedTouches[0];
    const dx = t.clientX - s.x, dy = t.clientY - s.y;
    if (Math.abs(dx) < 60 || Math.abs(dx) < 2 * Math.abs(dy) || Date.now() - s.at > 600) return;
    const sel = window.getSelection && window.getSelection();
    if (sel && !sel.isCollapsed) return; // selecting text, not swiping
    const next = tabs[tabs.indexOf(cur) + (dx < 0 ? 1 : -1)];
    if (next) { visit(next); next.click(); } // spa.js takes it from here, pill and all
  }, { passive: true });
}
