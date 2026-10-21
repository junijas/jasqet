const APP = 'http://127.0.0.1:9614';
// The store editions set this to false: store rules don't allow YouTube
// downloading in extensions (the Jasqet app itself still handles YouTube links).
const YOUTUBE = true;

const DEFAULTS = {
  enabled: true,
  catchMagnet: true,
  ytSession: false, // use this browser's YouTube session when YouTube asks "are you a robot?"
  prefetch: true,   // get video qualities ready before you click
  catchAll: false,
  videoButton: true,
  types: 'zip rar 7z gz tar xz iso img exe msi apk xapk dmg deb rpm appimage mp4 mkv avi mov wmv flv webm m4v 3gp mp3 m4a flac wav aac ogg pdf epub docx xlsx pptx torrent bin',
};
const CATCH_MIME = /^(video|audio)\/|application\/(zip|x-zip|x-rar|vnd\.rar|x-7z|x-msdownload|x-msi|octet-stream|vnd\.android\.package-archive|x-iso9660|x-apple-diskimage|pdf|epub|x-tar|gzip|x-bittorrent)/i;

const getSettings = () => chrome.storage.local.get(DEFAULTS);

function extOf(s) {
  try { s = decodeURIComponent(new URL(s).pathname); } catch {}
  const m = /\.([a-z0-9]{1,8})$/i.exec((s || '').split(/[\\/]/).pop());
  return m ? m[1].toLowerCase() : '';
}

async function cookieString(url) {
  try {
    const cookies = await chrome.cookies.getAll({ url });
    return cookies.map(c => `${c.name}=${c.value}`).join('; ');
  } catch { return ''; }
}

async function headersFor(url, referrer) {
  const h = { 'User-Agent': navigator.userAgent };
  if (referrer) h.Referer = referrer;
  const c = await cookieString(url);
  if (c) h.Cookie = c;
  return h;
}

// noteProblem: what went wrong talking to Jasqet the last time, and how long
// it took. Kept in storage (lastProblem) and the console, so a vague "Jasqet
// isn't running" can be looked into.
function noteProblem(path, what, started) {
  const p = { at: new Date().toISOString(), path, what: String(what).slice(0, 300), ms: Date.now() - started };
  console.warn('Jasqet:', p);
  chrome.storage.local.set({ lastProblem: p }).catch(() => {});
}

// rawPost: send to whatever answers on Jasqet's address, without checking it.
// Only for what carries nothing private; the rest goes through post().
async function rawPost(path, body, timeout = 4000) {
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), timeout);
  const started = Date.now();
  try {
    const r = await fetch(APP + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: ctl.signal,
    });
    let j;
    try { j = await r.json(); } catch (e) {
      noteProblem(path, `unreadable answer (HTTP ${r.status}): ${e.name}: ${e.message}`, started);
      if (!r.ok) throw new Error('Jasqet said no');
      throw new Error('Jasqet\'s answer was cut off');
    }
    if (!r.ok) throw new Error(j.error || 'Jasqet said no');
    return j;
  } catch (e) {
    if (!/answer was cut off|said no/.test(e.message)) noteProblem(path, `${e.name}: ${e.message}`, started);
    throw e;
  } finally {
    clearTimeout(t);
  }
}

// ---------- pairing: is it really Jasqet? ----------
// Any program can listen on Jasqet's address while Jasqet isn't running, and
// would get your cookies and links. So the first time, Jasqet gives this
// extension a secret (pair). After that, before sending anything, the
// extension says hello with a random word (a nonce), and Jasqet must answer
// with a proof made from that word and the secret. A program without the
// secret can't make it, so it gets nothing, and the browser keeps its own
// download. The extension never swaps its secret by itself: only when you
// click Pair again in its menu (pairAgain).
const PROBLEMS = {
  app: 'Jasqet isn\'t running.',
  allow: 'Jasqet hasn\'t allowed this browser yet. Click Allow in the Jasqet window.',
  old: 'This Jasqet is too old for this extension, so nothing was sent. Update Jasqet, then try again.',
  pair: 'This isn\'t the Jasqet you paired with, so nothing was sent. Open Jasqet, then click the Jasqet icon in your browser and Pair again.',
  refused: 'Jasqet needs your OK to pair this browser, so nothing was sent. Click Pair in the Jasqet window, and this browser pairs by itself.',
};
const problem = p => ({ problem: p, message: PROBLEMS[p] });

// A good proof counts for two minutes, so asking YouTube for qualities (often
// 15 s) and then downloading don't each wait for a hello. Any failed request
// ends it at once (post), so when Jasqet closes the next request checks again.
const TRUST_FOR = 2 * 60e3;
let trustedUntil = 0;
let checking = null; // the check under way, shared by everything waiting for it

const hex = bytes => [...new Uint8Array(bytes)].map(b => b.toString(16).padStart(2, '0')).join('');
const unhex = s => new Uint8Array(s.match(/../g).map(b => parseInt(b, 16)));
const isHex64 = s => typeof s === 'string' && /^[0-9a-f]{64}$/.test(s);

// proofOK: is proof Jasqet's answer to nonce, HMAC-SHA256(secret, "jasqet-pair:" + nonce)?
// (Jasqet's side: pairProof in extauth.go.)
async function proofOK(secret, nonce, proof) {
  if (!isHex64(secret) || !isHex64(proof)) return false;
  const key = await crypto.subtle.importKey('raw', unhex(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['verify']);
  return crypto.subtle.verify('HMAC', key, unhex(proof), new TextEncoder().encode('jasqet-pair:' + nonce));
}

// hi: hello with a fresh nonce. Jasqet's answer (with the nonce), or null if
// nothing answers. One more try after a miss: a busy moment isn't "not running".
async function hi(id) {
  for (let i = 0; i < 2; i++) {
    const nonce = hex(crypto.getRandomValues(new Uint8Array(32)));
    try {
      const a = await rawPost('/api/ping', { nonce, id: id || '' }, 6000);
      if (a.app === 'Jasqet') return { ...a, nonce };
      noteProblem('/api/ping', 'an answer that isn\'t from Jasqet: ' + JSON.stringify(a).slice(0, 200), Date.now());
    } catch {}
  }
  return null;
}

// pair: ask Jasqet for a pairing and keep it. null if Jasqet said no.
async function pair() {
  try {
    const p = await rawPost('/api/pair', {}, 6000);
    if (!isHex64(p.secret) || !p.id) return null;
    const pairing = { id: String(p.id), secret: p.secret };
    await chrome.storage.local.set({ pairing });
    return pairing;
  } catch { return null; }
}

// verify: is the program on Jasqet's address the Jasqet we paired with?
// { ok: true }, or { problem, message } (problem is one of PROBLEMS).
async function verify() {
  let { pairing } = await chrome.storage.local.get('pairing');
  let hello = await hi(pairing?.id);
  if (!hello) return problem('app');
  if (hello.allowed === false) return problem('allow');
  if (!hello.canPair) return problem('old');
  if (!pairing) { // the first time: Jasqet gives us a secret, then proves it
    pairing = await pair();
    if (!pairing) { keepPairing(); return problem('refused'); }
    hello = await hi(pairing.id);
    if (!hello) return problem('app');
  }
  if (!(await proofOK(pairing.secret, hello.nonce, hello.proof))) return problem('pair');
  trustedUntil = Date.now() + TRUST_FOR;
  return { ok: true };
}

// keepPairing: Jasqet said no because it already knows this browser (the
// extension was removed and added back, or its data was cleared). Jasqet now
// asks you in its window; this tries again every few seconds, so pairing
// happens by itself once you click Pair there. Two minutes, like Pair again.
let pairingUntil = 0;
function keepPairing() {
  if (Date.now() < pairingUntil) return; // already trying
  pairingUntil = Date.now() + 120e3;
  const tick = async () => {
    const { pairing } = await chrome.storage.local.get('pairing'); // (this also keeps the worker awake)
    if (pairing || Date.now() > pairingUntil) { pairingUntil = 0; return; }
    if (await pair()) { pairingUntil = 0; trustedUntil = 0; checkJasqet(); return; }
    setTimeout(tick, 3000);
  };
  setTimeout(tick, 3000);
}

function checkJasqet() {
  if (Date.now() < trustedUntil) return Promise.resolve({ ok: true });
  checking ||= verify().catch(() => problem('app')).finally(() => { checking = null; });
  return checking;
}

// pairAgain: you clicked Pair again in the popup. Forget the old pairing and
// ask for a new one (Jasqet agrees for a new browser, or after you click Pair
// again in its Connect window).
async function pairAgain() {
  await checking;
  await chrome.storage.local.remove('pairing');
  trustedUntil = 0;
  return checkJasqet();
}

// post: hand something to Jasqet, once it has proved it's the Jasqet we paired with.
async function post(path, body, timeout = 4000) {
  const c = await checkJasqet();
  if (!c.ok) throw new Error(c.message);
  try { return await rawPost(path, body, timeout); }
  catch (e) { trustedUntil = 0; throw e; } // check again next time
}

// Hello when the extension starts: Jasqet notes it's installed (and the first time, they pair).
checkJasqet();

async function sendToApp(url, { referrer = '', fileName = '', page = '' } = {}) {
  try {
    const headers = /^magnet:/i.test(url) ? { 'User-Agent': navigator.userAgent } : await headersFor(url, referrer);
    // page: where the link came from, so Jasqet can get a fresh link if this one expires
    await post('/api/add', { url, fileName, headers, page: page || referrer }, /^magnet:|\.torrent(\?|$)/i.test(url) ? 30000 : 2500);
    return true;
  } catch { return false; }
}

async function showApp(page = '') {
  const send = /^(zip|add):/.test(page) ? post : rawPost; // a page with a link in it is private
  try { await send('/api/show', { page }, 2500); return true; }
  catch { return false; }
}

function flash(text, color, tabId) {
  const o = tabId ? { tabId } : {};
  chrome.action.setBadgeBackgroundColor({ ...o, color });
  chrome.action.setBadgeText({ ...o, text });
  setTimeout(() => updateBadge(tabId), 3000);
}

// ---------- catching normal downloads ----------
// The download Jasqet should take, or null.
async function toCatch(item) {
  const s = await getSettings();
  if (!s.enabled || item.incognito || item.byExtensionId === chrome.runtime.id) return null;
  if (item.state !== 'in_progress') return null;
  const url = item.finalUrl || item.url;
  if (!/^https?:/i.test(url)) return null; // blob:, data: etc. only exist inside the page
  const name = (item.filename || '').split(/[\\/]/).pop();
  const ext = extOf(name) || extOf(url);
  const wanted = s.catchAll || s.types.split(/\s+/).includes(ext) || (!ext && CATCH_MIME.test(item.mime || ''));
  if (!wanted || (item.fileSize > 0 && item.fileSize < 64 * 1024)) return null;
  return { url, name };
}

async function handOver(item, c) {
  let page = item.referrer || '';
  if (!page) { try { const [t] = await chrome.tabs.query({ active: true, lastFocusedWindow: true }); page = t?.url || ''; } catch {} }
  return sendToApp(c.url, { referrer: item.referrer, fileName: c.name, page });
}

// The browser's own copy goes once Jasqet has the download.
async function dropBrowserCopy(id) {
  try { await chrome.downloads.cancel(id); } catch {}
  try {
    const [it] = await chrome.downloads.search({ id });
    if (it?.state === 'complete') await chrome.downloads.removeFile(id); // it finished first: no second copy
  } catch {}
  try { await chrome.downloads.erase({ id }); } catch {}
  flash('✓', '#13955b');
}

if (chrome.downloads.onDeterminingFilename) {
  // Chrome and Edge hold a new download until extensions have had their say
  // on its name: Jasqet takes it then, before the browser saves anything (a
  // small PDF used to finish first and end up downloaded twice).
  chrome.downloads.onDeterminingFilename.addListener((item, suggest) => {
    (async () => {
      try {
        const c = await toCatch(item);
        if (c && await handOver(item, c)) await dropBrowserCopy(item.id);
      } finally {
        try { suggest(); } catch {} // otherwise the browser saves it as usual
      }
    })();
    return true; // answered later
  });
} else {
  // Firefox has no such step: hold the browser's copy while Jasqet answers.
  chrome.downloads.onCreated.addListener(async item => {
    const c = await toCatch(item);
    if (!c) return;
    let held = false;
    try { await chrome.downloads.pause(item.id); held = true; } catch {}
    if (await handOver(item, c)) await dropBrowserCopy(item.id);
    else if (held) { try { await chrome.downloads.resume(item.id); } catch {} } // Jasqet isn't there: the browser carries on
  });
}

// ---------- spotting videos while pages play them ----------
// Video players fetch a playlist (.m3u8 / .mpd) or a plain video file. We note
// those per tab so the "Download video" button can offer them.
// YouTube's own chunk requests are skipped: YouTube downloads go through the site helper.
const SKIP_HOST = /(^|\.)(googlevideo\.com|youtube\.com|youtube-nocookie\.com|ytimg\.com|doubleclick\.net|googlesyndication\.com|googleadservices\.com)$/i;
const tabMedia = new Map();

async function loadTab(tabId) {
  if (!tabMedia.has(tabId)) {
    const key = 't' + tabId;
    let saved = [];
    try { saved = (await chrome.storage.session.get(key))[key] || []; } catch {}
    if (!tabMedia.has(tabId)) tabMedia.set(tabId, new Map(saved));
  }
  return tabMedia.get(tabId);
}

function saveTab(tabId) {
  const m = tabMedia.get(tabId);
  if (m) chrome.storage.session.set({ ['t' + tabId]: [...m] }).catch(() => {});
}

function header(d, name) {
  const h = (d.responseHeaders || []).find(h => h.name.toLowerCase() === name);
  return h ? h.value || '' : '';
}

function classify(d) {
  if (d.tabId < 0 || d.statusCode >= 400) return null;
  let u;
  try { u = new URL(d.url); } catch { return null; }
  if (!/^https?:$/.test(u.protocol) || SKIP_HOST.test(u.hostname)) return null;
  const path = u.pathname.toLowerCase();
  const ct = header(d, 'content-type').toLowerCase();
  if (path.endsWith('.m3u8') || /mpegurl/.test(ct)) return { kind: 'hls' };
  if (path.endsWith('.mpd') || ct.includes('dash+xml')) return { kind: 'dash' };
  const isMedia = /^(video|audio)\//.test(ct) && !/mp2t|mpegurl|dash/.test(ct);
  const extMedia = /\.(mp4|webm|mkv|mov|m4v|flv|mp3|m4a|ogg|wav)$/.test(path);
  if (!isMedia && !(extMedia && (!ct || ct.includes('octet-stream')))) return null;
  // chunks of a stream, not whole files
  if (/\.(ts|m4s|aac|vtt|cmfv|cmfa)$/.test(path) || /[?&](range|bytestart|byteend|sq)=/.test(u.search) || /\/(seg|segment|chunk|frag)[-_]?\d/i.test(path)) return null;
  const m = /\/(\d+)\s*$/.exec(header(d, 'content-range'));
  const size = m ? +m[1] : (+header(d, 'content-length') || 0);
  if (size && size < 1024 * 1024) return null;
  return { kind: 'file', size, mime: ct.split(';')[0] };
}

chrome.webRequest.onHeadersReceived.addListener(d => {
  const c = classify(d);
  if (!c) return;
  loadTab(d.tabId).then(m => {
    const prev = m.get(d.url);
    m.set(d.url, { url: d.url, ...c, at: Date.now(), size: Math.max(c.size || 0, prev?.size || 0) });
    if (m.size > 40) { // keep the newest
      const oldest = [...m.values()].sort((a, b) => a.at - b.at)[0];
      m.delete(oldest.url);
    }
    saveTab(d.tabId);
    if (!prev) updateBadge(d.tabId);
  });
}, { urls: ['<all_urls>'], types: ['xmlhttprequest', 'media', 'other', 'object'] }, ['responseHeaders']);

chrome.tabs.onUpdated.addListener((tabId, info) => {
  if (info.status === 'loading' && info.url) { // a new page: forget the old one's videos
    tabMedia.set(tabId, new Map());
    saveTab(tabId);
    updateBadge(tabId);
  }
});
chrome.tabs.onRemoved.addListener(tabId => {
  tabMedia.delete(tabId);
  chrome.storage.session.remove('t' + tabId).catch(() => {});
});

async function updateBadge(tabId) {
  if (!tabId) { chrome.action.setBadgeText({ text: '' }); return; }
  const m = await loadTab(tabId);
  // A stream's playlists (one per quality) count as one video.
  const items = [...m.values()];
  const streams = items.some(i => i.kind !== 'file') ? 1 : 0;
  const n = streams + items.filter(i => i.kind === 'file' && (!streams || !i.size || i.size > 20e6)).length;
  try {
    chrome.action.setBadgeBackgroundColor({ tabId, color: '#006fef' });
    chrome.action.setBadgeTextColor?.({ tabId, color: '#ffffff' });
    chrome.action.setBadgeText({ tabId, text: n ? String(Math.min(n, 9)) + (n > 9 ? '+' : '') : '' });
  } catch {}
}

function hostOf(u) { try { return new URL(u).hostname.toLowerCase(); } catch { return ''; } }

// The site a host belongs to ("cdn.example.co.uk" → "example.co.uk").
function siteOf(h) {
  const p = (h || '').split('.');
  const n = p.length >= 3 && p[p.length - 1].length === 2 && p[p.length - 2].length <= 3 ? 3 : 2;
  return p.slice(-n).join('.');
}

async function cookiesForHosts(mainURL, hosts) {
  const out = {};
  const scheme = mainURL.startsWith('http:') ? 'http:' : 'https:';
  const main = hostOf(mainURL);
  for (const h of new Set([main, ...(hosts || [])])) {
    if (!h || siteOf(h) !== siteOf(main)) continue; // a playlist can name any site: its cookies stay home
    const c = await cookieString(h === main ? mainURL : `${scheme}//${h}/`);
    if (c) out[h] = c;
  }
  return out;
}

function pageHeaders(referer) {
  const h = { 'User-Agent': navigator.userAgent };
  if (referer && /^https?:/.test(referer)) {
    h.Referer = referer;
    try { h.Origin = new URL(referer).origin; } catch {}
  }
  return h;
}

function cleanTitle(t) {
  t = (t || '').replace(/^\(\d+\)\s*/, '').replace(/\s+-\s+YouTube$/, '').replace(/\s+/g, ' ').trim();
  return t.length > 120 ? t.slice(0, 120).trim() : t;
}

// What the video menus show when Jasqet isn't there, or isn't the one we paired with.
const notReady = c => c.problem === 'app' ? { error: 'app' } : { ytError: c.message };

async function getOptions(tabId, srcs, referer) {
  const c = await checkJasqet();
  if (!c.ok) return notReady(c);
  const m = await loadTab(tabId);
  const items = [...m.values()].sort((a, b) => b.at - a.at);
  for (const s of srcs || []) {
    if (/^https?:/.test(s) && !m.has(s) && !SKIP_HOST.test(hostOf(s))) {
      const kind = /\.m3u8(\?|$)/i.test(s) ? 'hls' : /\.mpd(\?|$)/i.test(s) ? 'dash' : 'file';
      items.push({ url: s, kind, at: 0 });
    }
  }
  const manifests = items.filter(i => i.kind !== 'file').slice(0, 8);
  const probes = await Promise.all(manifests.map(async i => {
    try {
      // a video the page only names (not one it really loaded) gets cookies only from the page's own site
      const mine = m.has(i.url) || siteOf(hostOf(i.url)) === siteOf(hostOf(referer));
      const info = await post('/api/media/probe', {
        url: i.url, headers: pageHeaders(referer), cookies: mine ? await cookiesForHosts(i.url, []) : {},
      }, 20000);
      return { item: i, info };
    } catch (e) { return { item: i, error: e.message }; }
  }));
  const covered = new Set();
  for (const p of probes) for (const u of p.info?.mediaPlaylists || []) covered.add(u);
  const groups = [];
  for (const p of probes) {
    if (covered.has(p.item.url)) continue; // a quality of a playlist we already list
    if (p.info) {
      groups.push({ kind: p.info.kind, url: p.item.url, duration: p.info.duration, drm: p.info.drm, live: p.info.live, hosts: p.info.hosts, variants: p.info.variants, budget: p.info.budget });
    }
  }
  const errors = probes.filter(p => p.error).map(p => p.error);
  for (const f of items.filter(i => i.kind === 'file')) {
    if (groups.length && f.size && f.size < 20e6) continue; // probably part of a stream
    groups.push({ kind: 'file', url: f.url, size: f.size || 0, mime: f.mime || '' });
  }
  return { groups, errors };
}

const isYT = u => { try { return /(^|\.)youtube\.com$|(^|\.)youtube-nocookie\.com$|^youtu\.be$/i.test(new URL(u).hostname); } catch { return false; } };

// Your YouTube session (this browser's YouTube cookies). Only sent when you
// allowed it, and Jasqet only uses it when YouTube asks "are you a robot?".
async function ytSessionCookie() {
  try {
    const list = await chrome.cookies.getAll({ domain: 'youtube.com' });
    return list.map(c => `${c.name}=${c.value}`).join('; ');
  } catch { return ''; }
}
async function ytCookies(url, session) {
  if (!isYT(url)) return cookiesForHosts(url, []);
  const s = await getSettings();
  if (!(session || s.ytSession)) return {};
  const c = await ytSessionCookie();
  return c ? { 'youtube.com': c } : {};
}

// YouTube and other video sites go through the site helper (yt-dlp) in the app.
// Answers are kept for a few minutes, so the menu opens at once.
const ytCache = new Map();
async function ytOptions(pageURL, session = false, fresh = false) {
  const c = await checkJasqet(); // every time, so the menu works as soon as you pair
  if (!c.ok) return notReady(c);
  const key = pageURL + (session ? '#s' : '');
  const hit = ytCache.get(key);
  // kept 10 minutes; a failure 20 s; while its list is still being read, 3 s
  if (hit && !fresh && Date.now() - hit.at < (hit.ok ? (hit.loading ? 3e3 : 10 * 60e3) : 20e3)) return hit.promise;
  const promise = (async () => {
    try {
      const p = await post('/api/yt/probe', { url: pageURL, cookies: await ytCookies(pageURL, session) }, 150000);
      return { yt: p, session };
    } catch (e) { return { ytError: e.message || 'The site helper could not read this page', robot: /robot/i.test(e.message || ''), session }; }
  })();
  const entry = { at: Date.now(), promise, ok: false };
  ytCache.set(key, entry);
  promise.then(r => {
    if (r.yt?.setup) { // the helper is still being set up: ask afresh next time
      if (ytCache.get(key) === entry) ytCache.delete(key);
      return;
    }
    entry.ok = !!r.yt; entry.loading = !!r.yt?.playlistLoading; entry.at = Date.now();
  });
  if (ytCache.size > 60) ytCache.delete(ytCache.keys().next().value);
  return promise;
}

async function download(opt, referer, title, later) {
  title = cleanTitle(title);
  try {
    if (opt.kind === 'yt') {
      await post('/api/yt/add', {
        url: opt.url, variant: opt.variant, quality: opt.label, title: opt.title || title, estSize: opt.estSize || 0,
        later: !!later, playlist: opt.playlist || null, headers: pageHeaders(referer), subs: opt.subs || '',
        cookies: await ytCookies(opt.url, opt.session),
      });
      return { ok: true };
    }
    if (opt.kind === 'file') {
      let ext = extOf(opt.url);
      if (!ext) ext = (opt.mime || '').includes('webm') ? 'webm' : (opt.mime || '').startsWith('audio') ? 'mp3' : 'mp4';
      const h = await headersFor(opt.url, referer);
      await post('/api/add', { url: opt.url, fileName: title ? `${title}.${ext}` : '', headers: h, later: !!later });
    } else {
      await post('/api/media/add', {
        url: opt.url, variant: opt.variant, quality: opt.label, title,
        audioOnly: !!opt.audioOnly, estSize: opt.estSize || 0, later: !!later,
        headers: pageHeaders(referer), cookies: await cookiesForHosts(opt.url, opt.hosts),
      });
    }
    return { ok: true };
  } catch (e) {
    return { ok: false, error: e.message || 'Could not reach Jasqet' };
  }
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg?.type === 'tg-options') {
    const tabId = msg.tabId ?? sender.tab?.id;
    const page = msg.referer || sender.url;
    (async () => {
      if (msg.remember) await chrome.storage.local.set({ ytSession: true });
      if (isYT(page)) return YOUTUBE ? ytOptions(page, !!msg.session) : { ytError: 'In this edition, copy the YouTube link and paste it into Jasqet → Add link.' };
      return getOptions(tabId, msg.srcs, page);
    })().then(sendResponse);
    return true;
  }
  if (msg?.type === 'tg-yt-probe') {
    (async () => {
      if (msg.remember) await chrome.storage.local.set({ ytSession: true });
      return ytOptions(msg.url, !!msg.session);
    })().then(sendResponse);
    return true;
  }
  if (msg?.type === 'tg-tools-install') {
    post('/api/tools/install', {}).then(sendResponse, e => sendResponse({ state: 'error', message: e.message }));
    return true;
  }
  if (msg?.type === 'tg-tools') {
    fetch(APP + '/api/tools').then(r => r.json()).then(sendResponse, () => sendResponse({ state: 'error', message: 'Jasqet isn\'t running' }));
    return true;
  }
  if (msg?.type === 'tg-download') {
    const tabId = msg.tabId ?? sender.tab?.id;
    download(msg.opt, msg.referer || sender.url, msg.title || sender.tab?.title || '', msg.later).then(r => {
      if (r.ok) flash('✓', '#13955b', tabId);
      sendResponse(r);
    });
    return true;
  }
  if (msg?.type === 'tg-open-batch') {
    openBatch(msg.tabId);
    return;
  }
  if (msg?.type === 'tg-batch') {
    (async () => {
      try {
        const hosts = [...new Set(msg.urls.map(hostOf).filter(Boolean))].slice(0, 30);
        const cookies = {};
        for (const h of hosts) {
          const c = await cookieString(msg.urls.find(u => hostOf(u) === h));
          if (c) cookies[h] = c;
        }
        const r = await post('/api/add', { urls: msg.urls.join('\n'), later: !!msg.later, cookies, page: msg.referer || '', headers: { 'User-Agent': navigator.userAgent, Referer: msg.referer || '' } }, 15000);
        sendResponse({ ok: true, count: r.ids.length });
      } catch (e) { sendResponse({ ok: false, error: e.message }); }
    })();
    return true;
  }
  if (msg?.type === 'tg-settings') {
    getSettings().then(sendResponse);
    return true;
  }
  if (msg?.type === 'tg-magnet') { // a magnet link was clicked on a page
    sendToApp(msg.url, {}).then(ok => { flash(ok ? '✓' : 'OFF', ok ? '#13955b' : '#d23c3c', sender.tab?.id); sendResponse({ ok }); });
    return true;
  }
  if (msg?.type === 'tg-show') {
    showApp(msg.page || '').then(ok => sendResponse({ ok }));
    return true;
  }
  if (msg?.type === 'tg-check') { // the popup's status: check afresh
    trustedUntil = 0;
    checkJasqet().then(sendResponse);
    return true;
  }
  if (msg?.type === 'tg-pair-again') {
    pairAgain().then(sendResponse);
    return true;
  }
  if (msg?.type === 'tg-sizes') { // sizes for "files over 1 MB" in the link picker
    (async () => {
      try {
        const hosts = [...new Set(msg.urls.map(hostOf).filter(Boolean))].slice(0, 30);
        const cookies = {};
        for (const h of hosts) { const c = await cookieString(msg.urls.find(u => hostOf(u) === h)); if (c) cookies[h] = c; }
        const r = await post('/api/sizes', { urls: msg.urls, cookies, headers: { 'User-Agent': navigator.userAgent, Referer: msg.referer || '' } }, 60000);
        sendResponse({ ok: true, sizes: r.sizes });
      } catch (e) { sendResponse({ ok: false, error: e.message }); }
    })();
    return true;
  }
});

// ---------- right-click menu ----------
// The callbacks take any error themselves: when the extension is reloaded while
// its background restarts, the browser otherwise reports "No SW" on its Errors page.
function makeMenus() {
  const quiet = () => void chrome.runtime.lastError;
  chrome.contextMenus.removeAll(() => {
    quiet();
    chrome.contextMenus.create({ id: 'tg-link', title: 'Download with Jasqet', contexts: ['link'] }, quiet);
    chrome.contextMenus.create({ id: 'tg-zip', title: 'Look inside this ZIP with Jasqet (take out just one file)', contexts: ['link'],
      targetUrlPatterns: ['*://*/*.zip', '*://*/*.zip?*', '*://*/*.ZIP', '*://*/*.apk', '*://*/*.epub'] }, quiet);
    chrome.contextMenus.create({ id: 'tg-media', title: 'Download this media with Jasqet', contexts: ['video', 'audio', 'image'] }, quiet);
    chrome.contextMenus.create({ id: 'tg-page', title: 'Download links on this page with Jasqet…', contexts: ['page', 'selection'] }, quiet);
  });
}
chrome.runtime.onInstalled.addListener(makeMenus);
chrome.runtime.onStartup.addListener(makeMenus);

function openBatch(tabId) {
  chrome.tabs.create({ url: chrome.runtime.getURL('batch.html?tab=' + tabId) });
}

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === 'tg-page') { openBatch(tab.id); return; }
  if (info.menuItemId === 'tg-zip') { const ok = await showApp('zip:' + info.linkUrl); flash(ok ? '✓' : 'OFF', ok ? '#13955b' : '#d23c3c', tab?.id); return; }
  const url = info.menuItemId === 'tg-link' ? info.linkUrl : info.srcUrl;
  if (url && /^magnet:/i.test(url)) { const ok = await sendToApp(url, {}); flash(ok ? '✓' : 'OFF', ok ? '#13955b' : '#d23c3c', tab?.id); return; }
  if (!url || !/^https?:/i.test(url)) {
    // blob: video: point people to the video button instead
    flash('▶', '#006fef', tab?.id);
    return;
  }
  const ok = await sendToApp(url, { referrer: info.pageUrl || tab?.url || '', page: info.pageUrl || tab?.url || '' });
  flash(ok ? '✓' : 'OFF', ok ? '#13955b' : '#d23c3c', tab?.id);
});
