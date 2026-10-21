// Jasqet: puts a "Download video" button on video players, with a quality menu.
(() => {
  if (window.__jasqetLoaded) return;
  window.__jasqetLoaded = true;

  // "Download all links on this page": hand the page's links to the picker.
  try {
    chrome.runtime.onMessage.addListener((msg, sender, reply) => {
      if (msg?.type !== 'tg-collect' || window !== window.top) return;
      const links = [];
      const add = (url, text) => { if (url) links.push({ url, text: (text || '').trim().replace(/\s+/g, ' ').slice(0, 200) }); };
      document.querySelectorAll('a[href], area[href]').forEach(a => add(a.href, a.textContent || a.title));
      document.querySelectorAll('video[src], audio[src], source[src]').forEach(v => add(v.src, ''));
      document.querySelectorAll('img[src]').forEach(i => { if (i.naturalWidth >= 200 || i.width >= 200) add(i.currentSrc || i.src, i.alt); });
      reply({ page: location.href, title: document.title, links });
    });
  } catch {}

  // Magnet links go to Jasqet (it downloads torrents too).
  let catchMagnet = true;
  try {
    chrome.storage.local.get({ catchMagnet: true, enabled: true }).then(s => { catchMagnet = s.catchMagnet !== false && s.enabled !== false; });
    chrome.storage.onChanged.addListener(ch => { if (ch.catchMagnet) catchMagnet = ch.catchMagnet.newValue !== false; });
  } catch {}
  document.addEventListener('click', e => {
    if (!e.isTrusted || !catchMagnet || e.defaultPrevented || e.button !== 0) return; // only your own clicks, not the page's
    const a = e.target.closest && e.target.closest('a[href^="magnet:"]');
    if (!a) return;
    e.preventDefault(); e.stopPropagation();
    try { chrome.runtime.sendMessage({ type: 'tg-magnet', url: a.href }); } catch {}
  }, true);

  const MIN_W = 200, MIN_H = 110;
  let enabled = true;
  let host, root, btn, menu, current, hideTimer, lastMove = 0;
  let menuOpen = false, options = [], later = false, lastAsk = null, lastRes = null;
  let subsLang = '', prefetchOn = true, remember = true;
  const prefetched = new Map(); // what we already asked about, so the menu opens at once

  try {
    chrome.storage.local.get({ videoButton: true, later: false, subsLang: '', prefetch: true }).then(s => { enabled = s.videoButton !== false; later = !!s.later; subsLang = s.subsLang || ''; prefetchOn = s.prefetch !== false; });
    chrome.storage.onChanged.addListener(ch => {
      if (ch.videoButton) { enabled = ch.videoButton.newValue !== false; if (!enabled) hide(); }
      if (ch.prefetch) prefetchOn = ch.prefetch.newValue !== false;
    });
  } catch {}

  // the arrow from Jasqet's logo
  const ICON = '<svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v13m-6-6 6 6 6-6M5 21h14" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';

  function ui() {
    if (host) return;
    host = document.createElement('jasqet-ui');
    host.style.cssText = 'all:initial;position:fixed;left:0;top:0;width:0;height:0;z-index:2147483647;';
    root = host.attachShadow({ mode: 'closed' });
    root.innerHTML = `<style>
      :host{all:initial}
      .b{position:fixed;display:none;align-items:center;gap:6px;height:32px;padding:0 13px 0 10px;border-radius:16px;border:0;
         background:#006fef;color:#fff;font:600 13px/1 "Segoe UI",system-ui,-apple-system,sans-serif;cursor:pointer;
         box-shadow:0 2px 12px rgba(0,0,0,.4);opacity:.95;transition:opacity .15s, transform .15s;white-space:nowrap}
      .b:hover{opacity:1;transform:translateY(-1px)}
      .m{position:fixed;display:none;width:280px;max-height:62vh;overflow:auto;background:#161a21;color:#e9ecf1;
         border:1px solid #2c3340;border-radius:12px;box-shadow:0 12px 40px rgba(0,0,0,.55);
         font:13px/1.35 "Segoe UI",system-ui,-apple-system,sans-serif;padding:6px;box-sizing:border-box}
      .h{padding:9px 10px 4px;font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:#8c94a3;display:flex;justify-content:space-between}
      .i{display:flex;justify-content:space-between;align-items:center;gap:12px;width:100%;border:0;background:none;color:inherit;
         font:inherit;text-align:left;padding:9px 10px;border-radius:8px;cursor:pointer;box-sizing:border-box}
      .i:hover:not([disabled]){background:#232934}
      .i b{font-weight:600}
      .i small{color:#8c94a3;font-variant-numeric:tabular-nums;white-space:nowrap}
      .i[disabled]{opacity:.55;cursor:default}
      .n{padding:12px 10px;color:#b4bbc7}
      .ok{color:#35c483;font-weight:600}
      .err{color:#ff8b8b}
      .sep{height:1px;background:#262c37;margin:4px 6px}
      .h .t{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;text-transform:none;letter-spacing:0;font-size:12px;color:#b4bbc7;max-width:200px}
      .i.act b{color:#3daeff}
      .lat{display:flex;align-items:center;gap:8px;padding:9px 10px 6px;color:#b4bbc7;font-size:12.5px;cursor:pointer;border-top:1px solid #262c37;margin-top:4px}
      .lat input{accent-color:#3daeff;margin:0}
      .subs{display:flex;align-items:center;justify-content:space-between;gap:8px;padding:6px 10px;color:#b4bbc7;font-size:12.5px}
      .subs select{flex:1;max-width:170px;background:#232934;color:#e9ecf1;border:1px solid #2c3340;border-radius:7px;padding:4px 6px;font:inherit}
    </style><button class="b" type="button">${ICON}<span>Download video</span></button><div class="m" role="menu"></div>`;
    btn = root.querySelector('.b');
    menu = root.querySelector('.m');
    btn.addEventListener('click', e => { e.preventDefault(); e.stopPropagation(); toggleMenu(); });
    menu.addEventListener('click', onMenuClick);
    menu.addEventListener('change', e => {
      const c = e.target.closest('[data-act="later"]');
      if (c) { later = c.checked; try { chrome.storage.local.set({ later }); } catch {} }
      const sb = e.target.closest('[data-act="subs"]');
      if (sb) { subsLang = sb.value; try { chrome.storage.local.set({ subsLang }); } catch {} }
      const rm = e.target.closest('[data-act="remember"]');
      if (rm) remember = rm.checked;
    });
    menu.addEventListener('click', e => { if (e.target.closest('select,label')) e.stopPropagation(); });
    attach();
  }

  function attach() {
    const parent = document.fullscreenElement || document.documentElement;
    if (host && host.parentNode !== parent) parent.appendChild(host);
  }
  document.addEventListener('fullscreenchange', () => { if (host) { attach(); place(); } }, true);

  function visible(v) {
    const r = v.getBoundingClientRect();
    return r.width >= MIN_W && r.height >= MIN_H && r.bottom > 0 && r.right > 0 && r.top < innerHeight && r.left < innerWidth;
  }
  function videoAt(x, y) {
    for (const v of document.querySelectorAll('video')) {
      if (!visible(v)) continue;
      const r = v.getBoundingClientRect();
      if (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) return v;
    }
    return null;
  }

  document.addEventListener('mousemove', e => {
    if (!enabled) return;
    const now = performance.now();
    if (now - lastMove < 100) return;
    lastMove = now;
    const v = videoAt(e.clientX, e.clientY);
    if (v) show(v);
    else if (current && !menuOpen) scheduleHide();
  }, { passive: true, capture: true });

  document.addEventListener('play', e => {
    const v = e.target;
    if (enabled && v instanceof HTMLVideoElement && visible(v)) { show(v); scheduleHide(4000); }
  }, true);

  addEventListener('scroll', () => place(), { passive: true, capture: true });
  addEventListener('resize', () => place(), { passive: true });
  document.addEventListener('click', e => {
    if (menuOpen && host && !e.composedPath().includes(host)) { closeMenu(); scheduleHide(); }
  }, true);
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && menuOpen) closeMenu(); }, true);

  function show(v) {
    ui();
    attach();
    current = v;
    clearTimeout(hideTimer);
    btn.style.display = 'flex';
    place();
    schedulePrefetch();
  }

  // Ask for the qualities as soon as the button shows (hover or play), so they
  // are ready when you click. Once per video, and only while the tab is visible.
  let prefetchTimer = null;
  function askFor() {
    const srcs = [];
    if (current) {
      for (const s of [current.currentSrc, current.src, ...[...current.querySelectorAll('source')].map(x => x.src)]) {
        if (s && /^https?:/.test(s)) srcs.push(s);
      }
    }
    return { type: 'tg-options', srcs, referer: location.href };
  }
  // usable: an answer the menu can show (not an error, not the helper still setting up)
  function usable(r) { return !!r && !r.error && !r.ytError && !r.yt?.setup && !!(r.yt || (r.groups || []).length); }
  function prefetchKey() { return location.href + '|' + (isYT() ? '' : (current?.currentSrc || '')); }
  function isYT() { return /(^|\.)youtube(-nocookie)?\.com$|^youtu\.be$/.test(location.hostname); }
  function schedulePrefetch() {
    if (!prefetchOn || document.hidden || prefetchTimer) return;
    const key = prefetchKey();
    if (prefetched.has(key)) return;
    prefetchTimer = setTimeout(() => {
      prefetchTimer = null;
      if (document.hidden || prefetched.has(key) || prefetchKey() !== key) return;
      if (isYT() && !/\/(watch|shorts\/|live\/|embed\/)/.test(location.pathname + '/') && !/[?&]v=/.test(location.search)) return; // not a video page
      const msg = askFor();
      const e = { msg };
      e.promise = ask(msg).then(r => { e.done = usable(r); return r; });
      prefetched.set(key, e);
      if (prefetched.size > 30) prefetched.delete(prefetched.keys().next().value);
    }, isYT() ? 900 : 400);
  }

  function place() {
    if (!current || !btn) return;
    if (!current.isConnected || !visible(current)) { if (!menuOpen) hide(); return; }
    const r = current.getBoundingClientRect();
    const bw = btn.offsetWidth || 160;
    btn.style.left = Math.max(4, Math.min(innerWidth - bw - 4, r.right - bw - 12)) + 'px';
    btn.style.top = Math.max(4, r.top + 12) + 'px';
    if (menuOpen) placeMenu();
  }

  function placeMenu() {
    const b = btn.getBoundingClientRect();
    const mw = menu.offsetWidth || 280;
    menu.style.left = Math.max(4, Math.min(innerWidth - mw - 4, b.right - mw)) + 'px';
    const below = b.bottom + 6;
    menu.style.top = (below + Math.min(menu.offsetHeight, innerHeight * .62) > innerHeight ? Math.max(4, b.top - 6 - menu.offsetHeight) : below) + 'px';
  }

  function scheduleHide(ms = 1600) {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => { if (!menuOpen) hide(); }, ms);
  }
  function hide() {
    if (btn) btn.style.display = 'none';
    closeMenu();
    current = null;
  }
  function closeMenu() {
    menuOpen = false;
    if (menu) menu.style.display = 'none';
  }

  async function ask(msg) {
    try { return await chrome.runtime.sendMessage(msg); } catch { return { error: 'ext' }; }
  }

  async function toggleMenu() {
    if (menuOpen) { closeMenu(); return; }
    menuOpen = true;
    clearTimeout(hideTimer);
    menu.style.display = 'block';
    const yt = /(^|\.)youtube(-nocookie)?\.com$|^youtu\.be$/.test(location.hostname);
    menu.innerHTML = `<div class="n">${yt ? 'Asking YouTube for qualities… (a few seconds)' : 'Looking for video qualities…'}</div>`;
    const ready = prefetched.get(prefetchKey());
    const quick = yt && !ready?.done && TGMenu.quickYT(location.href, document.title);
    if (quick) show_(quick); // the usual choices at once; the exact list replaces them when YouTube answers
    placeMenu();
    const srcs = [];
    if (current) {
      for (const s of [current.currentSrc, current.src, ...[...current.querySelectorAll('source')].map(x => x.src)]) {
        if (s && /^https?:/.test(s)) srcs.push(s);
      }
    }
    lastAsk = { type: 'tg-options', srcs, referer: location.href };
    const pre = prefetched.get(prefetchKey());
    let res = pre ? await pre.promise : null;
    if (!usable(res)) {
      res = await ask(lastAsk); // nothing ready (or it failed): ask now
      prefetched.set(prefetchKey(), { msg: lastAsk, promise: Promise.resolve(res), done: usable(res) });
    }
    if (!menuOpen) return;
    showOrSetup(res);
    if (res?.yt?.playlistLoading) waitForList();
  }

  // waitForList: the video's qualities are shown; its list (a Mix can take a
  // minute) is added when Jasqet has read it, while the menu stays open.
  function waitForList(tries = 30) {
    setTimeout(async () => {
      if (!menuOpen || tries <= 0) return;
      const r = await ask(lastAsk);
      if (!menuOpen) return;
      if (r?.yt && !r.yt.playlistLoading) {
        prefetched.set(prefetchKey(), { msg: lastAsk, promise: Promise.resolve(r), done: usable(r) });
        show_(r);
      } else waitForList(tries - 1);
    }, 4000);
  }

  // The YouTube & site helper sets itself up the first time you ask for a
  // video (only asking ahead, before you click, never starts it).
  function showOrSetup(res) {
    if (res?.yt?.setup && res.yt.setup.state !== 'error') setupTools();
    else show_(res);
  }

  function show_(res) {
    lastRes = res;
    const out = TGMenu.render(res, { later, page: location.href, subsLang });
    options = out.options;
    menu.innerHTML = out.html;
    placeMenu();
  }

  let settingUp = false;
  async function setupTools() {
    if (settingUp) return;
    settingUp = true;
    try {
      show_({ yt: { setup: { state: 'installing', progress: 0 } } });
      await ask({ type: 'tg-tools-install' });
      while (menuOpen) { // closing the menu stops watching; the setup carries on in Jasqet
        const st = await ask({ type: 'tg-tools' });
        if (st.state === 'ready') break;
        if (st.state !== 'installing') { show_({ yt: { setup: { ...st, state: 'error', message: st.message || st.error || 'The setup didn\'t start.' } } }); return; }
        show_({ yt: { setup: st } });
        await new Promise(r => setTimeout(r, 1200));
      }
      if (!menuOpen) return;
      menu.innerHTML = '<div class="n">Ready! Asking for qualities…</div>';
      const res = await ask(lastAsk);
      prefetched.set(prefetchKey(), { msg: lastAsk, promise: Promise.resolve(res), done: usable(res) });
      if (menuOpen) show_(res);
    } finally {
      settingUp = false;
    }
  }

  async function onMenuClick(e) {
    const act = e.target.closest('.act');
    if (act) {
      e.stopPropagation();
      if (act.dataset.act === 'setup') { setupTools(); return; }
      if (act.dataset.act === 'session') {
        menu.innerHTML = '<div class="n">Asking YouTube again with your session…</div>';
        lastAsk = { ...lastAsk, session: true, remember };
        const res = await ask(lastAsk);
        prefetched.set(prefetchKey(), { msg: lastAsk, promise: Promise.resolve(res), done: usable(res) });
        showOrSetup(res);
        return;
      }
      if (act.dataset.act === 'helper') {
        menu.innerHTML = '<div class="n">Asking the site helper… (a few seconds)</div>';
        lastAsk = { type: 'tg-yt-probe', url: location.href };
        showOrSetup(await ask(lastAsk));
        return;
      }
    }
    const b = e.target.closest('.i');
    if (!b || b.disabled || b.dataset.i === undefined) return;
    e.stopPropagation();
    const opt = { ...options[+b.dataset.i], subs: subsLang, session: !!lastRes?.session };
    if (opt.subsOnly && !subsLang) {
      const sel = menu.querySelector('[data-act="subs"]');
      if (sel) { sel.focus(); sel.style.outline = '2px solid #3daeff'; }
      return;
    }
    menu.innerHTML = '<div class="n">Sending to Jasqet…</div>';
    const r = await ask({ type: 'tg-download', opt, referer: location.href, later });
    menu.innerHTML = r?.ok
      ? `<div class="n ok">✓ ${later ? 'Scheduled in Jasqet' : 'Downloading in Jasqet'}</div>`
      : `<div class="n err">${TGMenu.esc(r?.error || 'Couldn\'t reach Jasqet. Reload the page and try again.')}</div>`;
    setTimeout(() => { closeMenu(); scheduleHide(800); }, r?.ok ? 1400 : 3500);
  }
})();
