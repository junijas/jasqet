// Lists every link on a page so you can pick which files to download.
const $ = id => document.getElementById(id);
const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const TYPES = {
  Video: 'mp4 mkv avi mov wmv flv webm m4v 3gp ts mpg mpeg',
  Music: 'mp3 wav flac aac m4a ogg wma opus',
  Archives: 'zip rar 7z tar gz bz2 xz tgz iso img',
  Programs: 'exe msi apk xapk dmg deb rpm appimage msix pkg',
  Documents: 'pdf doc docx xls xlsx ppt pptx txt epub csv odt rtf',
  Images: 'jpg jpeg png gif webp svg bmp heic tiff',
};
const extOf = u => { try { const m = /\.([a-z0-9]{1,8})$/i.exec(decodeURIComponent(new URL(u).pathname)); return m ? m[1].toLowerCase() : ''; } catch { return ''; } };
const typeOf = u => { const e = extOf(u); for (const [t, list] of Object.entries(TYPES)) if (list.split(' ').includes(e)) return t; return 'Other'; };
const nameOf = (u, text) => { try { const n = decodeURIComponent(new URL(u).pathname.split('/').pop()); return n || text || u; } catch { return text || u; } };

const params = new URLSearchParams(location.search);
const tabId = +params.get('tab');
let links = [], filter = 'Files', selected = new Set(), pageURL = '', sizes = {};
const fmtB = n => { if (!(n >= 0)) return ''; const u = ['B', 'KB', 'MB', 'GB']; let i = 0; while (n >= 1024 && i < 3) { n /= 1024; i++; } return (i ? n.toFixed(n < 10 ? 1 : 0) : n) + ' ' + u[i]; };

async function load() {
  let res;
  try { res = await chrome.tabs.sendMessage(tabId, { type: 'tg-collect' }, { frameId: 0 }); } catch {}
  if (!res) { $('main').innerHTML = '<div class="msg">Couldn\'t read that page. Reload it and try again.</div>'; return; }
  pageURL = res.page;
  $('src').textContent = res.title ? `${res.title} · ${res.page}` : res.page;
  const seen = new Set();
  for (const l of res.links) {
    if (!/^(https?|ftp):/i.test(l.url) || seen.has(l.url)) continue;
    seen.add(l.url);
    links.push({ ...l, type: typeOf(l.url), ext: extOf(l.url), name: nameOf(l.url, l.text) });
  }
  // Default: show files (not ordinary web pages) and pre-select them.
  for (const l of links) if (l.type !== 'Other') selected.add(l.url);
  if (!links.some(l => l.type !== 'Other')) filter = 'All';
  draw();
}

function shown() {
  const q = $('q').value.trim().toLowerCase();
  return links.filter(l => (filter === 'All' || (filter === 'Files' ? l.type !== 'Other' : l.type === filter)) &&
    (!q || (l.name + ' ' + l.url + ' ' + (l.text || '')).toLowerCase().includes(q)));
}

function draw() {
  const counts = { Files: links.filter(l => l.type !== 'Other').length, All: links.length };
  for (const t of [...Object.keys(TYPES), 'Other']) counts[t] = links.filter(l => l.type === t).length;
  $('chips').innerHTML = ['Files', ...Object.keys(TYPES), 'Other', 'All'].filter(t => counts[t] || t === 'Files' || t === 'All')
    .map(t => `<button class="chip ${filter === t ? 'on' : ''}" data-t="${t}">${t === 'Files' ? 'All files' : t === 'Other' ? 'Other links' : t === 'All' ? 'Everything' : t}<small>${counts[t]}</small></button>`).join('');
  const rows = shown();
  if (!rows.length) { $('main').innerHTML = '<div class="msg">No links match.</div>'; }
  else $('main').innerHTML = `<table><tr><th style="width:36px"></th><th>Name</th><th>Type</th>${Object.keys(sizes).length ? '<th>Size</th>' : ''}<th>Address</th></tr>${rows.map(l =>
    `<tr><td><input type="checkbox" data-u="${esc(l.url)}" ${selected.has(l.url) ? 'checked' : ''}></td><td class="n">${esc(l.name)}${l.text && l.text !== l.name ? `<div style="font-weight:400;color:var(--muted);font-size:12px">${esc(l.text.slice(0, 120))}</div>` : ''}</td><td class="t">${l.type === 'Other' ? (extOf(l.url) || 'page') : extOf(l.url)}</td>${Object.keys(sizes).length ? `<td class="s">${sizes[l.url] > 0 ? fmtB(sizes[l.url]) : sizes[l.url] === -1 ? '?' : ''}</td>` : ''}<td class="u">${esc(l.url)}</td></tr>`).join('')}</table>`;
  $('all').checked = rows.length > 0 && rows.every(l => selected.has(l.url));
  const n = selected.size;
  $('count').textContent = n ? `${n} selected` : 'Nothing selected';
  $('go').disabled = $('later').disabled = !n;
  $('go').textContent = n ? `Download ${n}` : 'Download selected';
}

$('chips').onclick = e => { const c = e.target.closest('.chip'); if (c) { filter = c.dataset.t; draw(); } };
$('q').oninput = draw;
$('main').onchange = e => { const c = e.target.closest('[data-u]'); if (c) { c.checked ? selected.add(c.dataset.u) : selected.delete(c.dataset.u); draw(); } };
$('all').onchange = e => { for (const l of shown()) e.target.checked ? selected.add(l.url) : selected.delete(l.url); draw(); };

async function send(later) {
  $('go').disabled = $('later').disabled = true;
  const r = await chrome.runtime.sendMessage({ type: 'tg-batch', urls: [...selected], referer: pageURL, later });
  $('count').innerHTML = r?.ok ? `<span class="ok">✓ ${r.count} ${later ? 'scheduled' : 'added'} in Jasqet</span>` : `<span class="err">${esc(r?.error || 'Couldn\'t reach Jasqet. Is the app running?')}</span>`;
  if (!r?.ok) $('go').disabled = $('later').disabled = false;
}
async function ask() {
  const text = $('ask').value.trim(); if (!text) return;
  const q = SmartPick.parse(text);
  const out = $('askOut'); out.style.display = '';
  const say = (html) => { out.innerHTML = html; };
  const tags = SmartPick.describe(q, fmtB).map(t => `<span class="tag">${esc(t)}</span>`).join('');
  let candidates = SmartPick.apply({ ...q, min: 0, max: 0, first: 0, last: 0 }, links, null);
  if ((q.min || q.max) && candidates.length) {
    const need = candidates.map(l => l.url).filter(u => !(u in sizes)).slice(0, 400);
    if (need.length) {
      say(`Understood: ${tags}. Checking the size of ${need.length} file${need.length == 1 ? '' : 's'} (this uses almost no data)…`);
      const r = await chrome.runtime.sendMessage({ type: 'tg-sizes', urls: need, referer: pageURL });
      if (!r?.ok) { say(`<span class="err">${esc(r?.error || "Couldn't reach Jasqet to check sizes. Is the app running?")}</span>`); return; }
      Object.assign(sizes, r.sizes);
    }
  }
  const picked = SmartPick.apply(q, links, sizes);
  selected = new Set(picked.map(l => l.url));
  filter = 'All';
  $('q').value = '';
  const unknown = (q.min || q.max) ? candidates.filter(l => !(sizes[l.url] > 0)).length : 0;
  say(`Understood: ${tags} → <b>${picked.length} file${picked.length == 1 ? '' : 's'} selected</b>${unknown ? ` · ${unknown} left out because the server didn't say how big ${unknown == 1 ? 'it is' : 'they are'}` : ''}. Untick anything you don't want.`);
  draw();
  // show the selection first
  const first = document.querySelector('#main input:checked'); if (first) first.scrollIntoView({ block: 'center' });
}
$('askGo').onclick = ask;
$('ask').addEventListener('keydown', e => { if (e.key === 'Enter') ask(); });
$('go').onclick = () => send(false);
$('later').onclick = () => send(true);
load();
