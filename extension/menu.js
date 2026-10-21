// Shared by the in-page "Download video" menu and the toolbar popup.
var TGMenu = (() => {
  const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  const fmtB = n => { if (!(n > 0)) return ''; const u = ['B', 'KB', 'MB', 'GB']; let i = 0; while (n >= 1024 && i < 3) { n /= 1024; i++; } return (i > 1 ? n.toFixed(n < 10 ? 1 : 0) : Math.round(n)) + ' ' + u[i]; };
  const fmtD = s => { if (!(s > 0)) return ''; s = Math.round(s); const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), ss = s % 60; return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(ss).padStart(2, '0'); };

  // a quality's size; with a data budget in Jasqet, more than the data left says so
  const size = (est, left) => !est ? '' : '~' + fmtB(est) + (left >= 0 && est > left ? ' <span class="err">· more than your data</span>' : '');

  function item(options, opt, label, right) {
    const i = options.push(opt) - 1;
    return `<button class="i" data-i="${i}"><b>${esc(label)}</b><small>${right || ''}</small></button>`;
  }

  // Turns the background's answer into menu HTML plus the list of choices.
  // subsLang: the subtitle language picked last time (kept between videos)
  function render(res, { later = false, page = '', subsLang = '' } = {}) {
    const options = [];
    if (!res) res = { error: 'ext' };
    if (res.error === 'app') return { html: '<div class="n"><b>Jasqet isn\'t running.</b><br>Open the Jasqet app, then try again.</div>', options };
    if (res.error) return { html: '<div class="n">The extension was just updated. Reload this page and try again.</div>', options };
    let h = '';
    const yt = res.yt;
    if (yt) {
      if (yt.setup) {
        // the helper sets itself up the first time you ask for a video (content.js and popup.js start it)
        const st = yt.setup;
        if (st.state === 'error') {
          h += `<div class="n"><b>The YouTube &amp; site helper couldn't be set up.</b><br><span class="err">${esc(st.message)}</span></div>`;
          h += '<button class="i act" data-act="setup"><b>Try again</b><small></small></button>';
        } else {
          const msg = st.message && st.message !== 'Starting…' ? st.message : `Jasqet is downloading two free helper tools (about ${st.sizeMB || 60} MB).`;
          h += `<div class="n"><b>Setting up the YouTube &amp; site helper (one time only)… ${Math.round((st.progress || 0) * 100)}%</b><br><small>${esc(msg)}<br>The qualities appear when it's done.</small></div>`;
        }
        return { html: h, options };
      }
      if (yt.title) h += `<div class="h"><span class="t">${esc(yt.title)}</span><span>${fmtD(yt.duration)}</span></div>`;
      if (yt.quick) h += '<div class="n"><small>Sizes, subtitles and the playlist are on their way. You can pick now.</small></div>';
      const subs = yt.subs || [];
      const pick = subs.find(s => s.lang === subsLang) || subs.find(s => subsLang && s.lang.replace(/-orig$/, '') === subsLang.replace(/-orig$/, ''));
      if (subs.length && !yt.drm && !yt.live) {
        h += `<label class="subs">Subtitles <select data-act="subs"><option value="">None</option>${subs.map(s => `<option value="${esc(s.lang)}" ${pick && pick.lang === s.lang ? 'selected' : ''}>${esc(s.name)}</option>`).join('')}</select></label>`;
      }
      if (yt.drm) h += '<button class="i" disabled><span>Can\'t be saved (protected by DRM)</span></button>';
      else if (yt.live) h += '<button class="i" disabled><span>Can\'t be saved yet (live or upcoming)</span></button>';
      else for (const v of yt.variants || []) {
        h += item(options, { kind: 'yt', url: yt.url, title: yt.title, variant: v.id, label: v.label, estSize: v.estSize }, v.label, size(v.estSize, yt.budget));
      }
      if (yt.playlistLoading && !yt.playlist) h += '<div class="sep"></div><div class="n"><small>Reading the whole playlist…</small></div>';
      if (yt.playlist && yt.playlist.count > 1) {
        const pl = yt.playlist;
        h += `<div class="sep"></div><div class="h"><span class="t">Whole playlist · ${pl.count} videos</span><span></span></div>`;
        const heights = [...new Set((yt.variants || []).map(v => v.height).filter(x => x > 0))];
        const picks = (heights.length ? heights : [1080, 720, 480, 360]).filter(x => x <= 1080).slice(0, 4);
        for (const ht of picks) h += item(options, { kind: 'yt', url: yt.url, playlist: pl, variant: 'h:' + ht, label: ht + 'p' }, `All ${pl.count} in ${ht}p`, '');
        h += item(options, { kind: 'yt', url: yt.url, playlist: pl, variant: 'audio', label: 'Audio only' }, `All ${pl.count} as audio only`, '');
      }
      if (subs.length && !yt.drm && !yt.live) h += item(options, { kind: 'yt', url: yt.url, title: yt.title, variant: 'subs', label: 'Subtitles', subsOnly: true }, 'Only the subtitles (.srt)', 'tiny');
      if (!options.length && !yt.drm && !yt.live) h += '<div class="n">No downloadable quality found for this video.</div>';
    }
    if (res.ytError) h += `<div class="n err">${esc(res.ytError)}</div>`;
    if (res.robot && !res.session) {
      h += `<button class="i act" data-act="session"><b>Use my YouTube session</b><small>fixes it</small></button>
        <label class="lat" style="border:0;margin:0;padding-top:2px"><input type="checkbox" data-act="remember" checked> Always do this when YouTube asks</label>
        <div class="n" style="padding-top:2px;font-size:12px">Jasqet uses this browser's YouTube sign-in only when YouTube asks this question (works best if you're signed in to YouTube here). YouTube may limit accounts used a lot with download tools, so a spare account is safest.</div>`;
    } else if (res.robot) {
      h += '<div class="n" style="font-size:12px">YouTube refused even with your session. Try again in a while, or sign in to YouTube in this browser first.</div>';
    }
    const groups = res.groups || [];
    groups.forEach((g, gi) => {
      if (gi || h) h += '<div class="sep"></div>';
      if (groups.length > 1 || g.duration) h += `<div class="h"><span>${groups.length > 1 ? 'Video ' + (gi + 1) : 'Choose quality'}</span><span>${fmtD(g.duration)}</span></div>`;
      if (g.kind === 'file') { h += item(options, { kind: 'file', url: g.url, mime: g.mime }, 'Original file', fmtB(g.size)); return; }
      if (g.drm) { h += '<button class="i" disabled><span>Can\'t be saved (protected by DRM)</span></button>'; return; }
      if (g.live) { h += '<button class="i" disabled><span>Can\'t be saved yet (live stream)</span></button>'; return; }
      for (const v of g.variants) h += item(options, { kind: g.kind, url: g.url, variant: v.id, label: v.label, estSize: v.estSize, audioOnly: v.audioOnly, hosts: g.hosts }, v.label, size(v.estSize, g.budget));
    });
    if (!yt && !groups.length && !res.ytError) {
      h += '<div class="n">No video found yet. Let the video play for a few seconds, then try again.</div>';
    }
    if (!yt && page && !res.robot) h += `<div class="sep"></div><button class="i act" data-act="helper"><span>Not the right video? <b>Try the site helper</b></span><small></small></button>`;
    if (options.length) h += `<label class="lat"><input type="checkbox" data-act="later" ${later ? 'checked' : ''}> Download at the scheduled time</label>`;
    return { html: h, options };
  }

  // quickYT: the usual YouTube choices, shown the moment you click while
  // YouTube is still being asked (it can take 15 s). Jasqet picks the exact
  // streams when the download starts: the best one up to the height you chose.
  // Only for one video (a watch, shorts or youtu.be page).
  function quickYT(page, title) {
    let u;
    try { u = new URL(page); } catch { return null; }
    if (!/(^|\.)youtube(-nocookie)?\.com$|^youtu\.be$/.test(u.hostname)) return null;
    if (!(u.hostname === 'youtu.be' || /^\/(watch|shorts\/|live\/)/.test(u.pathname))) return null;
    title = String(title || '').replace(/^\(\d+\)\s*/, '').replace(/\s*-\s*YouTube$/, '').trim();
    return { yt: { quick: true, url: page, title, variants: [
      { id: 'best', label: 'Best quality' }, { id: 'h:1080', label: '1080p' }, { id: 'h:720', label: '720p' },
      { id: 'h:480', label: '480p' }, { id: 'h:360', label: '360p' }, { id: 'audio', label: 'Audio only' },
    ] } };
  }

  return { render, esc, fmtB, quickYT };
})();
