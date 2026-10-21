// Understands requests like "all PDFs over 1 MB" or "first 10 lecture videos"
// for the link picker. It follows simple rules: it looks for file types,
// sizes, counts, words and sites in what you type.
const SmartPick = (() => {
  const GROUPS = {
    Video: { words: ['video', 'videos', 'movie', 'movies', 'film', 'films', 'clip', 'clips', 'lecture video', 'lecture videos'], exts: 'mp4 mkv avi mov wmv flv webm m4v 3gp ts mpg mpeg' },
    Music: { words: ['music', 'song', 'songs', 'audio', 'audios', 'sound', 'sounds', 'podcast', 'podcasts', 'sermon', 'sermons', 'track', 'tracks'], exts: 'mp3 wav flac aac m4a ogg wma opus' },
    Archives: { words: ['archive', 'archives', 'zip', 'zips', 'compressed', 'rar', 'rars'], exts: 'zip rar 7z tar gz bz2 xz tgz' },
    Programs: { words: ['program', 'programs', 'app', 'apps', 'software', 'installer', 'installers', 'setup', 'setups', 'exe', 'apk', 'apks'], exts: 'exe msi apk xapk dmg deb rpm appimage msix pkg' },
    Documents: { words: ['document', 'documents', 'doc', 'docs', 'paper', 'papers', 'notes', 'slides', 'book', 'books', 'ebook', 'ebooks', 'handout', 'handouts', 'worksheet', 'worksheets', 'spreadsheet', 'spreadsheets'], exts: 'pdf doc docx xls xlsx ppt pptx txt epub csv odt rtf' },
    Images: { words: ['image', 'images', 'photo', 'photos', 'picture', 'pictures', 'pic', 'pics', 'wallpaper', 'wallpapers'], exts: 'jpg jpeg png gif webp svg bmp heic tiff' },
  };
  const EXT_WORDS = { pdfs: 'pdf', word: 'doc docx', excel: 'xls xlsx', powerpoint: 'ppt pptx', powerpoints: 'ppt pptx', ppts: 'ppt pptx', mp3s: 'mp3', mp4s: 'mp4', jpgs: 'jpg jpeg', pngs: 'png', epubs: 'epub', isos: 'iso', torrents: 'torrent', subtitles: 'srt vtt ass', subs: 'srt vtt ass' };
  const ALL_EXTS = new Set(Object.values(GROUPS).flatMap(g => g.exts.split(' ')).concat(['iso', 'img', 'torrent', 'srt', 'vtt', 'bin', 'json', 'xml']));
  const UNITS = { b: 1, byte: 1, bytes: 1, k: 1024, kb: 1024, kib: 1024, m: 1 << 20, mb: 1 << 20, mib: 1 << 20, meg: 1 << 20, megs: 1 << 20, g: 1 << 30, gb: 1 << 30, gib: 1 << 30, gig: 1 << 30, gigs: 1 << 30 };
  const NUM = { one: 1, two: 2, three: 3, four: 4, five: 5, six: 6, seven: 7, eight: 8, nine: 9, ten: 10, twenty: 20, fifty: 50, hundred: 100 };
  const STOP = new Set('everything anything stuff things items them those these new get download grab select give me i want need all the every each a an of on in this page site from files file links link with and or please that are is be which only just any some to for named called containing contain contains name names title'.split(' '));

  function parse(text) {
    let t = ' ' + text.toLowerCase().replace(/[“”]/g, '"').replace(/[‘’]/g, "'") + ' ';
    const q = { exts: new Set(), groups: [], min: 0, max: 0, first: 0, last: 0, words: [], not: [], site: '' };
    // quoted words: "week 3"
    t = t.replace(/(not|except|without|but not)\s+["']([^"']+)["']/g, (_, n, w) => { q.not.push(w.trim()); return ' '; });
    t = t.replace(/["']([^"']+)["']/g, (_, w) => { q.words.push(w.trim()); return ' '; });
    // sizes
    const num = '(\\d+(?:[.,]\\d+)?)\\s*(gb|gib|gigs?|g|mb|mib|megs?|m|kb|kib|k|bytes?|b)\\b';
    const sz = (n, u) => parseFloat(n.replace(',', '.')) * UNITS[u];
    t = t.replace(new RegExp('(?:over|above|bigger than|larger than|more than|greater than|at least|from|>=?|minimum|min)\\s*' + num, 'g'), (_, n, u) => { q.min = sz(n, u); return ' '; });
    t = t.replace(new RegExp('(?:under|below|smaller than|less than|lower than|at most|up to|<=?|maximum|max)\\s*' + num, 'g'), (_, n, u) => { q.max = sz(n, u); return ' '; });
    t = t.replace(new RegExp('between\\s*' + num + '\\s*and\\s*' + num, 'g'), (_, a, ua, b, ub) => { q.min = sz(a, ua); q.max = sz(b, ub); return ' '; });
    // counts
    const cnt = '(\\d+|' + Object.keys(NUM).join('|') + ')';
    const n = v => +v || NUM[v] || 0;
    t = t.replace(new RegExp('\\b(first|top)\\s+' + cnt + '\\b', 'g'), (_, w, v) => { q.first = n(v); return ' '; });
    t = t.replace(new RegExp('\\b(last|final|latest|newest)\\s+' + cnt + '\\b', 'g'), (_, w, v) => { q.last = n(v); return ' '; });
    // site
    t = t.replace(/\b(?:from|on|at)\s+((?:[a-z0-9-]+\.)+[a-z]{2,})\b/g, (_, h) => { q.site = h; return ' '; });
    // exclusions
    t = t.replace(/\b(?:not|except|without|but not|excluding)\s+([a-z0-9][\w.-]*)/g, (_, w) => { q.not.push(w); return ' '; });
    // "with lecture in the name", "named week", "containing exam"
    t = t.replace(/\bwith\s+([\w.-]+)\s+in\s+(?:the\s+)?(?:name|title|link)s?\b/g, (_, w) => { q.words.push(w); return ' '; });
    t = t.replace(/\b(?:named|called|containing|contain|contains|about|titled|matching)\s+([\w.-]+)/g, (_, w) => { q.words.push(w); return ' '; });
    // types: groups, extension words, ".ext", "ext files"
    for (const [g, def] of Object.entries(GROUPS)) {
      for (const w of def.words.sort((a, b) => b.length - a.length)) {
        const re = new RegExp('\\b' + w.replace(/ /g, '\\s+') + '\\b', 'g');
        if (re.test(t)) { if (!q.groups.includes(g)) q.groups.push(g); t = t.replace(re, ' '); }
      }
    }
    t = t.replace(/\b([a-z]+)\b/g, (m, w) => {
      if (EXT_WORDS[w]) { EXT_WORDS[w].split(' ').forEach(e => q.exts.add(e)); return ' '; }
      return m;
    });
    t = t.replace(/\.([a-z0-9]{1,5})\b/g, (_, e) => { q.exts.add(e); return ' '; });
    t = t.replace(/\b([a-z0-9]{2,5})s?\b(?=\s+files?\b|\s*$|\s+(?:and|or|,))/g, (m, e) => { if (ALL_EXTS.has(e)) { q.exts.add(e); return ' '; } return m; });
    for (const w of t.split(/[\s,]+/)) { if (ALL_EXTS.has(w)) q.exts.add(w); else if (ALL_EXTS.has(w.replace(/s$/, ''))) q.exts.add(w.replace(/s$/, '')); }
    // anything else meaningful is a word to look for
    for (const w of t.split(/[\s,]+/)) {
      if (w.length > 2 && !STOP.has(w) && !ALL_EXTS.has(w) && !ALL_EXTS.has(w.replace(/s$/, '')) && !/^\d+$/.test(w) && !q.words.includes(w)) q.words.push(w);
    }
    return q;
  }

  function describe(q, fmt) {
    const parts = [];
    const types = [...q.groups.map(g => g.toLowerCase()), ...[...q.exts].map(e => '.' + e)];
    parts.push(types.length ? types.join(' or ') : 'all files');
    if (q.min) parts.push('over ' + fmt(q.min));
    if (q.max) parts.push('under ' + fmt(q.max));
    if (q.words.length) parts.push('with ' + q.words.map(w => `"${w}"`).join(' and ') + ' in the name');
    if (q.not.length) parts.push('not ' + q.not.map(w => `"${w}"`).join(', '));
    if (q.site) parts.push('from ' + q.site);
    if (q.first) parts.push('the first ' + q.first);
    if (q.last) parts.push('the last ' + q.last);
    return parts;
  }

  // apply works on links {url, name, text, type, ext}
  function apply(q, links, sizes) {
    let out = links.filter(l => {
      const hay = (l.name + ' ' + l.url + ' ' + (l.text || '')).toLowerCase();
      const typeOK = (!q.groups.length && !q.exts.size) ? l.type !== 'Other' : (q.groups.includes(l.type) || q.exts.has(l.ext));
      if (!typeOK) return false;
      if (q.words.length && !q.words.every(w => hay.includes(w))) return false;
      if (q.not.some(w => hay.includes(w))) return false;
      if (q.site) { try { if (!new URL(l.url).hostname.endsWith(q.site)) return false; } catch { return false; } }
      if ((q.min || q.max) && sizes) {
        const s = sizes[l.url];
        if (!(s > 0)) return false; // unknown size: leave it out (say so)
        if (q.min && s < q.min) return false;
        if (q.max && s > q.max) return false;
      }
      return true;
    });
    if (q.first) out = out.slice(0, q.first);
    if (q.last) out = out.slice(-q.last);
    return out;
  }
  return { parse, describe, apply };
})();
