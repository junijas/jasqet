const DEFAULTS = {
  enabled: true,
  catchMagnet: true,
  ytSession: false,
  prefetch: true,
  catchAll: false,
  videoButton: true,
  types: 'zip rar 7z gz tar xz iso img exe msi apk xapk dmg deb rpm appimage mp4 mkv avi mov wmv flv webm m4v 3gp mp3 m4a flac wav aac ogg pdf epub docx xlsx pptx torrent bin',
};
const $ = id => document.getElementById(id);

chrome.storage.local.get(DEFAULTS).then(s => {
  $('enabled').checked = s.enabled;
  $('catchAll').checked = s.catchAll;
  $('videoButton').checked = s.videoButton;
  $('catchMagnet').checked = s.catchMagnet !== false;
  $('prefetch').checked = s.prefetch !== false;
  $('ytSession').checked = !!s.ytSession;
  $('types').value = s.types;
  $('typesWrap').style.display = s.catchAll ? 'none' : '';
});
$('enabled').onchange = e => chrome.storage.local.set({ enabled: e.target.checked });
$('videoButton').onchange = e => chrome.storage.local.set({ videoButton: e.target.checked });
$('catchMagnet').onchange = e => chrome.storage.local.set({ catchMagnet: e.target.checked });
$('prefetch').onchange = e => chrome.storage.local.set({ prefetch: e.target.checked });
$('ytSession').onchange = e => chrome.storage.local.set({ ytSession: e.target.checked });
$('catchAll').onchange = e => { chrome.storage.local.set({ catchAll: e.target.checked }); $('typesWrap').style.display = e.target.checked ? 'none' : ''; };
$('types').onchange = e => chrome.storage.local.set({ types: e.target.value.toLowerCase().replace(/[.,]/g, ' ').trim() });
$('openApp').onclick = async () => {
  if (pairing) { // forget the old pairing and pair afresh (background.js pairAgain)
    $('openApp').disabled = true;
    $('stt').textContent = 'Pairing…';
    const c = await chrome.runtime.sendMessage({ type: 'tg-pair-again' }).catch(() => null);
    $('openApp').disabled = false;
    showStatus(c);
    if (c?.ok) load();
    else if (c?.problem === 'refused') chrome.runtime.sendMessage({ type: 'tg-show' }).catch(() => {}); // Jasqet asks there
    return;
  }
  await chrome.runtime.sendMessage({ type: 'tg-show' }).catch(() => null);
  window.close();
};
$('grab').onclick = async () => {
  const [t] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (t) { chrome.runtime.sendMessage({ type: 'tg-open-batch', tabId: t.id }); window.close(); }
};

// Is Jasqet running, and is it the Jasqet this browser paired with? (background.js checkJasqet)
let pairing = false; // the main button pairs again instead of opening Jasqet
function showStatus(c) {
  c = c || { problem: 'app' };
  pairing = c.problem === 'pair'; // (refused: Jasqet asks you in its window, and this pairs by itself)
  clearTimeout(recheck);
  $('st').className = c.ok ? 'st on' : 'st off';
  $('stMsg').hidden = true;
  $('openApp').textContent = 'Open Jasqet';
  if (c.ok) { $('stt').textContent = 'Connected'; return; }
  if (c.problem === 'app') {
    $('stt').textContent = 'App not running';
    $('openApp').textContent = 'Start the Jasqet app first';
    return;
  }
  if (c.problem === 'allow') { // Jasqet asks you first (it only listens to its own extension)
    $('stt').textContent = 'Click Allow in Jasqet';
    $('openApp').textContent = 'Open Jasqet to allow this browser';
    return;
  }
  $('stt').textContent = c.problem === 'old' ? 'Update Jasqet' : 'Not paired';
  $('stMsg').textContent = {
    pair: 'This isn\'t the Jasqet you paired with, so nothing is sent to it. Pair again only while Jasqet is open.',
    refused: 'Jasqet needs your OK to pair this browser. Click Pair in the Jasqet window: this browser pairs by itself and this menu says Connected.',
  }[c.problem] || c.message || '';
  $('stMsg').hidden = false;
  if (pairing) $('openApp').textContent = 'Pair again';
  if (c.problem === 'refused') {
    $('openApp').textContent = 'Open Jasqet to pair';
    recheck = setTimeout(() => chrome.runtime.sendMessage({ type: 'tg-check' }).then(c2 => { showStatus(c2); if (c2?.ok) load(); }, () => {}), 3000);
  }
}
let recheck = 0; // while Jasqet waits for your OK, look again every few seconds
chrome.runtime.sendMessage({ type: 'tg-check' }).then(showStatus, () => showStatus(null));

let options = [], tab, later = false, lastHelper = false, subsLang = '', lastRes = null, remember = true;
chrome.storage.local.get({ later: false, subsLang: '' }).then(s => { later = !!s.later; subsLang = s.subsLang || ''; });
const ask = msg => chrome.runtime.sendMessage(msg).catch(() => ({ error: 'ext' }));
function showRes(res) {
  lastRes = res;
  const out = TGMenu.render(res, { later, page: tab?.url || '', subsLang });
  options = out.options;
  $('vids').innerHTML = out.html;
}
async function load() {
  [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab || !/^https?:/.test(tab.url || '')) { $('vids').innerHTML = '<div class="n">Open a page with a video.</div>'; return; }
  const yt = /(^|\.)youtube(-nocookie)?\.com$|^youtu\.be$/.test(new URL(tab.url).hostname);
  $('vids').innerHTML = `<div class="n">${yt ? 'Asking YouTube for qualities…' : 'Looking…'}</div>`;
  showOrSetup(await ask({ type: 'tg-options', tabId: tab.id, referer: tab.url }));
}
load();

// The YouTube & site helper sets itself up the first time you ask for a video.
function showOrSetup(res) {
  if (res?.yt?.setup && res.yt.setup.state !== 'error') setupTools();
  else showRes(res);
}
let settingUp = false;
async function setupTools() {
  if (settingUp) return;
  settingUp = true;
  try {
    showRes({ yt: { setup: { state: 'installing', progress: 0 } } });
    await ask({ type: 'tg-tools-install' });
    for (;;) { // closing the popup stops watching; the setup carries on in Jasqet
      const st = await ask({ type: 'tg-tools' });
      if (st.state === 'ready') break;
      if (st.state !== 'installing') { showRes({ yt: { setup: { ...st, state: 'error', message: st.message || st.error || 'The setup didn\'t start.' } } }); return; }
      showRes({ yt: { setup: st } });
      await new Promise(r => setTimeout(r, 1200));
    }
    if (lastHelper) showRes(await ask({ type: 'tg-yt-probe', url: tab.url })); else load();
  } finally {
    settingUp = false;
  }
}

$('vids').addEventListener('change', e => {
  const c = e.target.closest('[data-act="later"]');
  if (c) { later = c.checked; chrome.storage.local.set({ later }); }
  const sb = e.target.closest('[data-act="subs"]');
  if (sb) { subsLang = sb.value; chrome.storage.local.set({ subsLang }); }
  const rm = e.target.closest('[data-act="remember"]');
  if (rm) remember = rm.checked;
});
$('vids').onclick = async e => {
  const act = e.target.closest('.act');
  if (act?.dataset.act === 'setup') { setupTools(); return; } // Try again
  if (act?.dataset.act === 'session') {
    $('vids').innerHTML = '<div class="n">Asking YouTube again with your session…</div>';
    showOrSetup(await ask({ type: 'tg-options', tabId: tab.id, referer: tab.url, session: true, remember }));
    return;
  }
  if (act?.dataset.act === 'helper') {
    lastHelper = true;
    $('vids').innerHTML = '<div class="n">Asking the site helper…</div>';
    showOrSetup(await ask({ type: 'tg-yt-probe', url: tab.url }));
    return;
  }
  const b = e.target.closest('.i');
  if (!b || b.disabled || b.dataset.i === undefined) return;
  b.disabled = true;
  const opt = { ...options[+b.dataset.i], subs: subsLang, session: !!lastRes?.session };
  if (opt.subsOnly && !subsLang) { b.disabled = false; const sel = document.querySelector('[data-act="subs"]'); if (sel) sel.focus(); return; }
  const r = await ask({ type: 'tg-download', tabId: tab.id, opt, referer: tab.url, title: tab.title, later });
  b.innerHTML = r?.ok ? `<span class="ok">✓ ${later ? 'Scheduled' : 'Downloading'} in Jasqet</span>` : `<span class="err">${TGMenu.esc(r?.error || 'Couldn\'t reach Jasqet')}</span>`;
};
