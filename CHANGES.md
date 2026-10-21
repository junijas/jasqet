# Jasqet 1.6.2: fixes from a security review

1.6.1 was given a security review: the local server and its API, LAN sharing,
downloaded files and helper tools, the browser extension, saved secrets, the
TurboGet move and the installer. Nothing a website could use was found; these
were worth fixing before more people use Jasqet.

## Security

- **Only Jasqet's own window can use its local API.** Each start gives the
  window a random key, and other programs, other accounts on the PC and other
  extensions' tabs are refused. Saved keys and passwords (Telegram, VirusTotal,
  proxy, LAN code) no longer go back to the window. It shows them as dots, and
  they stay as they are unless you type new ones.
- **The site helper (yt-dlp) only takes web addresses.** Text pasted into Add
  with the site helper ticked went to yt-dlp as it was, so
  `--config-locations=…` was read as one of its options (it could load a file
  from another computer). Now only http/https addresses go in, always after `--`.
- **Torrents: unticked files go from the right folder.** When a torrent went
  into "Name (1)", the files you unticked were deleted from the older "Name"
  folder instead.
- **The "downloaded from the internet" mark goes on sooner and stays on:** as
  soon as a download finishes (before hashing and VirusTotal, which can take a
  while), still there after a "Move to" rule copies the file to another drive,
  and on a Mac as the quarantine mark Gatekeeper checks.
- **The safety badge:** programs and scripts (`.exe`, `.bat`, `.ps1`, `.vbs`,
  `.js`, `.hta`, `.iso` …) show "⚠ Check this file", never "✓ Checked";
  "✓ Checksum matches" is only for a checksum you typed (one found on the same
  website only shows the file isn't damaged); and a file that's dangerous for
  another reason is renamed to `.blocked` even when its checksum is wrong too.
- **The browser extension only talks to the real Jasqet.** It pairs with Jasqet
  once (each browser on its own), and before it sends cookies or links Jasqet
  must prove it's the same Jasqet. A program pretending to be Jasqet gets
  nothing, and the browser keeps its own download. After you remove the
  extension and add it back, Jasqet asks in its window: click **Pair**, and the
  browser pairs by itself. Older extensions keep working; the new one needs
  this Jasqet.
- **LAN sharing needs a group code.** Jasqet makes one when you turn sharing on
  (**Show code** to see or copy it), and it's saved encrypted. Files fetched
  with a login, cookies or a link with `?…` in it are never shared; links and
  names are no longer sent to other computers in clear; copies are checked at
  random places and against the checksum, and one that doesn't match comes
  from the internet instead. Slow or fake computers can't hold up downloads,
  and a request can't be replayed. YouTube videos aren't shared for now.
- **Archives can't fill your drive:** unpacking stops at 100 times the
  archive's size (anything up to 1 GB is always fine), at 100,000 files, or
  when less than 1 GB would stay free. A file taken out of a ZIP online stops
  at its listed size, and a video piece far bigger than real ones is refused.
  A dangerous file inside an archive now shows on the badge.
- **Smaller ones:** leftover cookie files from a crash are removed when Jasqet
  starts; a proxy password goes to yt-dlp in its environment, not on its
  command line, where other accounts can see it; and look-alike sites such as
  "evil-youtube-nocookie.com" no longer count as YouTube.

## Fixes

- **The "Download video" menu opens at once on YouTube.** The usual qualities
  (Best, 1080p, 720p, 480p, 360p, Audio only) are there the moment you click,
  and you can start a download straight away: Jasqet picks the exact streams
  when it starts. Sizes, subtitles and the playlist fill in when YouTube
  answers. A video opened from a Mix or a playlist no longer waits for the
  whole list (it took up to a minute); the list follows by itself.
- **The extension waits longer for Jasqet and tries again**, and a check
  counts for two minutes, so a busy moment no longer shows "Jasqet isn't
  running" or "too old".
- **Clear finished can be undone.** Its message has an **Undo** button, the
  entries only go for good after two minutes, and with more than 20 it asks for
  a second click.

- **Error messages:** a number in a file or folder name was taken for the
  server's answer, so a missing file in "Movies 2404" said "File not found on the
  server (404)".

## Look

- **Thinner scrollbars** in the window, the viewer and the extension, in the
  colours of light and dark mode.
- **The viewer follows light and dark mode** like the window (it was always
  dark). Videos still play on black, with their controls over the picture.
- **Plainer wording** everywhere. The subtitle maker is now called automatic
  subtitles, and the browser extension is 1.3.6.

## Not verified here

- The window's key was tested with a copy of Jasqet in a browser, not yet in
  Jasqet's own window (WebView2) or on a Mac or Linux.
- Pairing was tested against a real, a fake and an old Jasqet with the
  extension's own code, not yet in a real browser.

# Jasqet 1.6.1: security and Windows fixes from the 1.6.0 review

1.6.0 was reviewed on a real Windows 11 PC (WebView2 155): the installer, the
viewer, preview while downloading, notifications, safety checks, speed and
idle use. Everything below was found there or in the code review; the engine
and the preview logic are unchanged except where noted.

**TurboGet is now called Jasqet** (another app already has the name
TurboGet), with its own logo and blue colours. Nothing is lost: Jasqet moves
the list, settings, tools and torrents from `%AppData%\TurboGet` to
`%AppData%\Jasqet` the first time it starts, and installs over TurboGet in
its folder, so the browser extension already loaded from there keeps working.

## Security

- **Only Jasqet's own extension can talk to Jasqet.** Before, any
  extension installed in any browser, even one with no permissions, could
  read every setting (the Telegram bot token, the VirusTotal key, a proxy
  password), change them, delete downloads, and save a download with any name
  into any folder (for example a script into the Startup folder, or a fake
  yt-dlp.exe into the tools folder, which Jasqet then runs). Now:
  - the extension in Jasqet's folder is recognised by the ID browsers give
    it; any other (Firefox, or a copy loaded from elsewhere) is let in once
    you click **Allow** in the Jasqet window;
  - even an allowed extension can only hand over downloads and videos;
    settings, opening files, deleting and the rest belong to Jasqet's window;
  - only Jasqet's window chooses folders.
- **Logins stay with their site.** A site login or `Authorization` header was
  also sent to mirrors, to the page a link came from, to checksum pages and to
  refreshed links on other sites. Now it only goes to the download's own site.
- **Secrets are no longer saved in plain text.** Site cookies and logins, the
  Telegram token, the VirusTotal key and a proxy password are saved encrypted in
  state.json (Windows' own protection for this user, like the YouTube session).
  Lists saved by 1.6.0 load as before and are encrypted on the next save.
- **More file types ask before opening:** `.rdp` (opened a remote desktop
  connection to whatever the file says), `.theme`/`.themepack` (applied a
  Windows theme), `.wsb`, `.website`, `.search-ms`, `.msh*`, `.psc1` and others
  Windows treats as dangerous. Names ending in dots or spaces count as what
  Windows sees ("setup.exe ." is a program), and "movie.mp4 (1).exe" is still
  recognised as a program pretending to be a video.
- **File names:** Windows device names (`con.txt`, `NUL.mp4`, `COM1`) get a `_`
  in front, invisible right-to-left marks are removed (they made
  "invoice[RLO]fdp.exe" show as "invoiceexe.pdf"), and long names are no longer cut in the
  middle of a letter.
- **Torrents:** the safety check now looks at the names the files really get
  on disk (`setup.bat.` and `player.ex*e` were missed). A new torrent never
  lands on top of a file or folder that's already there (it goes into
  "Name (1)"), and Remove / Download again delete only the torrent's own files.
  Before, they deleted the whole folder, including anything else in it.
- **Unpacked archives** keep the "downloaded from the internet" mark on every
  file (SmartScreen and Office Protected View still check them), and names
  inside an archive can't make hidden streams (`notes.txt:evil`) or device
  names.
- **Subtitle languages** from a video site can't be used as a path.
- **LAN sharing:** the group code is never sent over the network (each request
  carries a short-lived signature instead; the code used to be broadcast every
  30 s), links are shared without their `?token=…` or login, a file you
  downloaded with a login or cookies is only offered to a computer that has
  the same link, and copies that can't be checked against the website need a
  group code. 1.6.1 doesn't see computers running older versions.
- **The YouTube helper tools** (yt-dlp, Deno) are checked against the SHA-256
  their projects publish before they're used.
- **The browser extension (1.3.5):** a page can't add magnet links by
  "clicking" them from a script, and cookies only go to the video's own site,
  not to any host a page or playlist names.
- Requests from other websites that carry no Origin (pictures, links) are
  refused outright, and request bodies are limited in size.
- Dependencies updated for known vulnerabilities (govulncheck): golang.org/x/net
  0.60.0, ulikunitz/xz 0.5.15, pion/dtls 3.1.4, pion/stun 3.1.5,
  gorilla/websocket 1.5.3.

## Efficiency

- The torrent engine (and its DHT) closes two minutes after the last torrent
  stops, instead of answering the torrent network for the rest of the session.
  Seeding stops when you remove the download.
- While downloading, the list is saved every 10 s instead of every 2 s (its
  progress only changes that often), and the network checks (adapters every
  2 s, Windows' metered flag through PowerShell every 5 min) only run while
  something downloads or waits.
- "Keep awake" no longer keeps the computer awake while downloads are paused
  for a metered network, a used-up budget or quiet mode, or waiting minutes to
  try again.
- Preview keeps its file open instead of reopening it for every 32 KB read.
- Torrents: pieces come in order only while the player is actually reading
  (paused or closed, the torrent goes back to normal after 20 s; playing again
  turns it back on).
- The viewer stops waiting for a download that was removed.
- A damaged or made-up video chunk can't make the re-wrapper read a whole film
  into memory.
- Smooth mode checks the connection once a minute while nothing downloads
  (was every 10 s).
- Quitting flushes every download first, so no progress is downloaded twice
  after an update or restart.

## Smarter, on your own computer

- **Automatic subtitles.** Right-click a finished video or recording → **Make
  subtitles** (or **Make English subtitles** to translate). Whisper
  (whisper.cpp, "base" model) listens on this computer and saves
  "Name.English (auto-generated).srt" next to the file, which the viewer shows. The sound
  is taken out with Windows' own decoders (MP4, M4A, AAC, MP3, WMA, WAV…; ffmpeg
  next to Jasqet for the rest). The engine and model (about 87 MB) download
  once, the first time, pinned to fixed SHA-256 checksums. The language is
  worked out from five samples across the recording: on a real 80-minute
  interview the first 30 seconds alone sounded "Yoruba" to Whisper (an
  accent), which spoiled everything; the samples said English, and the
  transcript came out right. Measured here: the interview's sound taken out in
  21 s, three minutes of speech written down in 20 s. Mac and Linux use an
  installed whisper.cpp.
- **Learns when your internet is fastest.** While downloading without any
  speed limit, Jasqet keeps the usual best speed of each hour of the day (24
  numbers, on this computer). The Scheduler shows them as a chart; when four
  hours are clearly faster (1.5× or more), a banner offers to start scheduled
  downloads then. "Not now" hides it for two weeks.
- **Qualities that fit your data.** With a data budget, qualities bigger than
  the data left are marked in the Add dialog and the extension's video menu,
  and the best one that fits is picked.
- **Tidy video names** (Settings → General, on): release-site names become
  "Some Movie (2021).mp4" and "The Office US - S02E05.mkv" (in a folder per
  series). Other names stay as they are.

## Usability

- **Connect your browser:** Chrome, Edge, Brave, Opera and Vivaldi don't let
  other programs open their extensions page (they show a new tab instead), so
  the button now copies its address (e.g. `edge://extensions`) and says to
  paste it into the address bar. **Copy its address** gives the extension
  folder's address for the Load unpacked window (it's in a hidden folder). The
  window no longer says "Not connected yet" after every restart: Jasqet
  remembers when the extension last checked in, and shows it.
- **Downloads are no longer saved twice.** A PDF or other quick download could
  finish in the browser before the extension had held it, so it ended up both
  in the browser's Downloads folder and in Jasqet. In Chrome and Edge the
  extension now takes a download while the browser is still choosing its
  name, before anything is saved; in Firefox the browser's copy is removed if
  it finished first.
- **Jasqet's own title bar** on Windows: Windows' grey caption strip is gone
  and the top bar has its own minimise, maximise/restore and close buttons.
  Drag the bar to move the window (to a screen edge to snap it), double-click
  it to maximise; the edges and corners still resize it. The viewer's top bar
  does the same while a file is open. The window also shows Jasqet's icon
  (in the title bar, the taskbar and Alt+Tab it was Windows' blank one).
- **Viewer volume up to 200%**, as in VLC, for quiet recordings and films
  with 5.1 sound (mixed down to stereo, which makes them quieter). Above 100%
  a limiter keeps loud parts from crackling; at 100% and below nothing changes.
- Watch rules: **Check now** no longer breaks over two lines, and looks
  greyed out until the rule is saved.
- **YouTube and other video sites set themselves up.** The first time you
  pick a video (**Download video** on a page, the extension's toolbar menu, or
  a link pasted into Add link), Jasqet downloads its two helper tools by
  itself (yt-dlp and Deno, about 63 MB, once), shows the progress, then the
  qualities. There is no **Set up now** step any more: on this PC the qualities
  appeared 50 s after the first click. A video, subtitles or a watched channel
  that needs the tools waits for the setup instead of failing. Just opening a
  video page still downloads nothing, and a failed setup says why, with
  **Try again**.
- **Notifications** are named Jasqet with its icon (they appeared as
  "Windows PowerShell"), and downloads that finish together share one
  notification instead of one each.
- File and folder errors are shown in plain words ("Couldn't use “x.zip”:
  Access is denied.") instead of Go's text ("open C:\…\x.zip: Access is denied.").
- The viewer's "This file could not be displayed" panel: the **Back to
  downloads** button was white on white.

## Preview while downloading

- **MP4s with their index at the end start faster** on servers that limit
  each connection. The index (190 KB for a 150 s 720p film) was fetched by
  one connection: preview steps were at least 128 KB, which left too little
  after the first for a second connection to join. Where a player reads the
  end of the file, the steps there are now about a second of one connection,
  so the connections fetch it together (still exact requests, connections
  kept). Elsewhere the steps are as before.
- A step's answer ending where it was asked no longer counts as a dropped
  connection: Go hands over the last bytes together with the end of the
  answer, and the connection waited 1 s before its next step.
- Measured against a local server allowing 25 KB/s per connection, 16
  connections, Preview at 20%, time until the whole index has arrived:

| Index at the end | 1.6.0 | 1.6.1 |
|---|---|---|
| 60 KB | 2.0 s | 1.7 s |
| 190 KB | 7.4 s | 1.2 to 1.4 s |
| 1 MB | 5.2 to 10.3 s | 4.0 to 5.8 s |

  (The lower figure with the player's first request closed, the higher with it
  still open.) In the viewer page on Windows, the same 190 KB film went from
  7.0 s to 3.1 s until it played; the finished file matched the original. The
  engine bench gives the same numbers as before.

## Installer

- `Jasqet.exe -quit` (used by the installer) waits until Jasqet has saved
  and closed; the installer also waits up to 10 s before forcing it, so a
  slow old copy isn't killed in the middle of saving.
- Uninstall also removes the notification registration and the viewer's
  re-wrapped TS/FLV copies in %TEMP%. Your downloads and settings are kept, as
  before.
- `jasqet.nsi` takes `-DTARGET=x86-unicode`: the NSIS download for Windows
  has no 64-bit stubs, so a build made on Windows is a 32-bit installer (it
  installs the same 64-bit Jasqet). The default stays 64-bit.
- Installing over TurboGet: Jasqet goes into TurboGet's folder, closes and
  removes TurboGet.exe, and its shortcuts, uninstall entry, start-with-Windows
  entry, `turboget://` links and notification name make way for Jasqet's.
  Jasqet also removes its own old `turboget://` registration when it starts
  from TurboGet's folder (the zip). Not yet tried on a real PC: the tests below
  were done before the rename.
- Tested on Windows 11: a fresh install; installing over a running 1.6.1 with a
  download in progress (it closed, saved everything and carried on byte for
  byte); upgrading a running 1.6.0 (closed and replaced; 1.6.0 itself only
  saves progress every 10 s, so a few seconds are downloaded again); and
  uninstalling while running (only your settings and downloads are left).

## Checked on Windows 11 (and not changed)

H.264/AAC MP4 (index at the start and at the end), MKV, WebM, M4A, MP3, PDF,
pictures and TS/FLV re-wrapping play in the WebView2 window; the notification
buttons work; the crash watch shows "This file could not be displayed" after a
hang while the list keeps working; the installer closes a running Jasqet
and installs over it, and a download in progress resumes byte for byte;
preview while downloading plays in under a second, says "Waiting for download:
45%" and carries on, jumps play in about 6.6 s, and the finished files
match; the engine bench gives the same numbers as 1.6.0. The tests pass under
the race detector on Windows (MinGW-w64 GCC 16).

# TurboGet 1.6.0: 1.5.0's engine with the full built-in viewer

This release joins the two lines that grew out of 1.3.1: the **download engine
review** from 1.5.0 (faster finishes, retrying by itself after network
problems, durable progress, free-space check; see 1.4.0 below) and the **full
built-in viewer** from the 1.4 line, which meets the viewer spec. 1.5.0's
lighter viewer is replaced.

## The viewer (from the 1.4 line)

- **Opens inside the TurboGet window** (← Downloads or Esc goes back), or in a
  window of its own (Settings → Viewer → Show files). Either way it runs in a
  separate process from the list and the downloads: a file that crashes or
  freezes it shows "This file could not be displayed" with Open externally.
- **Formats:** MP4, MKV, WebM, MOV, M4V; TS and FLV re-wrapped as MP4 (nothing
  re-encoded); MP3, M4A/AAC, FLAC, WAV, OGG, Opus with cover art and tags; JPG,
  PNG, GIF, WebP, BMP, SVG, AVIF; PDF with PDF.js.
- **Controls:** seek bar with thumbnails, speed 0.5 to 2×, sound-track and
  subtitle pickers (SRT, VTT, ASS/SSA files next to the video, and text
  subtitles inside MKV), full screen, keyboard; pictures zoom, fit, 1:1,
  rotate; PDFs with thumbnails, page jump, zoom, fit width, find, copy text.
- **Safety:** every Open checks what the file really is; a program, script,
  archive or web page dressed as a video/PDF is blocked with Delete and Show in
  folder. Open never runs a program without a separate "Run it" confirmation
  (1.5.0 ran setup.exe straight away). PDFs: no scripts, forms can't be sent,
  links show their full address and ask first. SVGs: no scripts or outside
  files. Damaged files offer Verify (checksum + 8 small pieces compared with
  the website) and Re-download.
- Right-click menu (Open, Open externally, Open with…, Show in folder), Open
  on the "finished" notification, open automatically when finished (off),
  remember where you stopped, every setting from the spec. Settings chosen in
  1.5.0 carry over.
- From 1.5.0: files being shown are opened so they can still be renamed, moved
  or deleted (Windows).

## Preview while downloading: both methods together

- 1.5.0 cut a piece exactly at the byte the player asked for. That's kept: an
  MP4's index at the end of the file, or a jump ahead, gets a connection at
  exactly that byte.
- From the 1.4 line: the connections now **follow each player**. Right after
  where it reads, they line up one after another in small pieces, so a server
  that limits each connection feeds the player with all of them. Several
  readers at once (a player reads the start and the index together) no longer
  crowd each other out.
- While someone watches, each connection asks for about a second's worth at a
  time and the next one starts exactly where that answer ends, so every
  answer is read to the end and its connection is kept for the next request.
  Watching a 60 MB film over a link with 150 ms delay took 17 connections;
  cutting requests short, as the first 1.6 build did, took 421 (each a new
  TLS handshake on a real site, and some file hosts limit how often you
  connect).
- The connections follow the player only while it's reading: a paused or
  closed player, or the viewer merely being open on the download, leaves the
  normal order after a few seconds. Not with a speed limit (it can't make
  anything faster) or over FTP (every piece would need a new login).
- If the connection at the playing point goes silent (a dead mobile
  connection, say), it's replaced after 4 s instead of waiting for the 30 s
  stall timeout (measured: the player waited 3.9 s, was 24.7 s). A piece a
  connection gave up on is tried again when the player needs it (at most
  3 times).
- Torrents (pieces in order) and video-site downloads with separate video and
  sound (played together until joined) can be previewed too.
- Measured against a server allowing 25 KB/s per connection, 16 connections,
  Preview at 20%:

| | 1.5.0 | 1.6.0 |
|---|---|---|
| MP4, index at the start | ~73 s (3 of 3 runs) | 0.7 s |
| MP4, index at the end | 3 s or ~73 s (2 runs each) | 2.6 s |
| Jump to 80 s at ~35% |  | 6.0 s |

  The finished file matched the original byte for byte every time.
- During an automatic retry the viewer says "trying again by itself in 40 s"
  (with Try now) instead of "paused".

## Installer

- Back to closing TurboGet by renaming the old program aside and retrying
  (the fix for "Error opening file for writing"); 1.5.0 had lost it.
- `turboget://` links (the notification buttons) are removed on uninstall.

## Checks

The engine bench gives the same results as 1.5.0 (table below). Both test
suites (the engine's and the viewer's) pass under the race detector, plus the
viewer's Chromium checks (inside the window and in its own window), preview
of HTTP downloads, a DASH video with separate sound and a torrent from a
local seeder. Not run here: Windows itself (WebView2, the notification
buttons, full screen filling the screen); the test browser can't decode
H.264/AAC (Edge can).

# TurboGet 1.5.0: built-in viewer

**Open** (or double-click) a finished video, song, picture or PDF and it opens
inside TurboGet instead of another program.

- **What opens here:** MP4/M4V/MOV (H.264), WebM, MP3, M4A, AAC, FLAC, WAV,
  Ogg/Opus, JPG, PNG, GIF, WebP, AVIF, BMP, SVG and PDF. Anything else (MKV, AVI,
  TS, HEIC, or a video the player can't decode) goes to its usual program, and
  **Open externally** is always in the viewer's top bar.
- **How it plays:** with the browser engine the window already is (WebView2 /
  Edge on Windows), so nothing is added to the app. This replaces the libmpv /
  PDFium plan in the spec: same result for these formats, no extra size.
- **Video:** remembers your place, loads `.srt` subtitles saved next to the
  video, keyboard controls (Space, ← →, ↑ ↓, M, F, Esc), next/previous file.
- **Pictures:** full size on click, rotate. **PDFs:** the browser's own PDF
  viewer (search, zoom, pages).
- **Preview while downloading** (eye button, normal downloads of videos and
  music). Bytes already on disk are served at once; for the rest the engine
  cuts the piece at the byte being played so a connection starts exactly
  there, adds one connection while someone watches, and if every connection is
  busy, the one working furthest from the playhead moves over. Jumping ahead
  works the same way. In testing (2 connections at 400 KB/s each), playback
  started within a second, and a jump to 80 s into a 2-minute video played
  within 4 s at 21% downloaded; the file still finished byte for byte.
- **Safety:** the file's type is read from its first bytes, never trusted from
  its name, and it's only sent to the page as a video, sound, picture or PDF
  (with `nosniff`, and in a sandbox except PDFs). A web page named "movie.mp4"
  is never shown, so it can't run inside the app where it could reach
  TurboGet's controls. Files blocked by the safety check don't open. Other
  websites can't load files from the viewer. On Windows, files are opened so
  they can still be renamed or moved while viewed (the download finishing, an
  after-download rule).
- **Settings → General:** on/off, per kind (videos, music, pictures, PDFs), and
  preview on/off.

Tested in headless Chromium (21 checks: playback, subtitles, keyboard, resume
position, pictures, PDF delivery, fallback, disguised files, cross-site
loading, next/previous, settings, preview and jumping ahead), plus Go unit
tests, all under the race detector. Not tested here: Windows WebView2 itself,
H.264 playback and the PDF viewer (the test browser lacks those codecs and the
viewer; Edge has them).

# TurboGet 1.4.0: download engine review

Faster finishes, downloads that survive long outages, and less wasted data.
Everything else (UI, extension, video, torrents) works as before.

## Measured (local test server, same machine, before → after)

| Situation | 1.3.1 | 1.4.0 |
|---|---|---|
| One connection crawls at 24 KB/s (32 MB, 8 connections) | 85.0 s | 3.9 s |
| One connection goes silent halfway (8 MB, 4 connections) | 9.6 s | 4.8 s |
| Internet drops for 100 s mid-download | **gave up with an error** | finishes by itself |
| 30 small files behind a redirect (100 ms round trips) | 9.3 s, 33 connections | 5.8 s, 3 connections |
| Redirecting link, 8 connections | 9 redirects, 2.93 s | 1 redirect, 2.83 s |
| Speed limit with slow connections (stall timeout scaled down 10×) | 11.3 MB used for an 8 MB file (+41%) | 8.0 MB (+0%) |
| Per-connection-capped host, flaky drops | unchanged | unchanged |

Every file was checked byte for byte. The bench is `bench_test.go`:
`go test -tags tgbench -run TestBench -v -timeout 30m`.

## Speed

- **Rate-aware work stealing** (`engine.go`: `claim`, `splitPointLocked`).
  - Before, a piece was only shared if it had more than 4 MB left. On servers that cap each connection, one connection often finished the last few MB alone while the others sat idle.
  - Now a free connection takes half of any piece that would still take more than about 3 s at its measured speed (pieces never smaller than 256 KB).
  - A connection crawling at under ⅕ of the typical speed, or silent for 4 s (12 s before its first byte), hands its whole remainder to a fresh connection. Its request is ended straight away. A stretch moves at most 3 times.
  - With a speed limit (or over FTP) the old coarse splitting is kept, since splitting can't help there.
- **Idle connections wait instead of quitting.** They are woken the moment a piece finishes, so they can pick up slow or failed pieces. This also removed up to 300 ms of delay at the end of each download.
- **Redirects followed once per run.** Pieces go straight to where the link led.
  - If that address fails (for example, signed links expire), the next try goes through the original link and isn't counted as a failure.
  - It isn't used with a site login or an `Authorization` header, so credentials never reach a server the original link didn't lead to by itself.
- **The probe's 1-byte answer is read to the end**, so the first piece reuses that connection.
- **TLS session cache**: new connections to the same server resume the TLS session.

## Robustness

- **Slow connections are no longer treated as stalled.**
  - The old reader waited to fill a 256 KB buffer, so any connection slower than about 8.5 KB/s, including time spent waiting on the speed limiter, was cut and reopened every 30 s. Each reopen threw away data that was already on its way (a speed limit, metered "slow" mode, a weak signal).
  - Now every byte resets the stall timer, waiting for the limiter doesn't count, and writes are batched (up to 256 KB, at least 4× a second).
- **Automatic retry** (`robust.go`).
  - A download that stops on a network problem re-queues itself, waiting 5 s, 10 s, 20 s, 40 s, 1 min, 2 min, then every 5 min: about 2.5 h, longer while it keeps making progress. It also retries immediately when the network changes.
  - These count as network problems: no internet, DNS failures, timeouts, resets, 5xx/429/408, FTP 421/425/426/450.
  - These do not: 4xx, a bad certificate, a full disk, or a file that changed on the server.
  - Servers that can't resume get 3 tries only, because each try starts from zero.
  - The UI shows *Trying again by itself in …*.
- **A piece that fails isn't handed to every connection in turn** (that took about 40 min with 32 connections). A connection on a non-resumable download also stops after 3 failed tries; before, it re-downloaded from zero forever on a link that kept dropping.
- **Unknown-size downloads cut off midway** are no longer marked complete with a short file. A restart from zero now empties the file first.
- **Durable progress.** Segment positions are noted, the `.part` file is flushed, and only then are those positions saved (at most every 10 s). After a power cut a resumed file can't contain a hole of zeros.
  - `state.json` is written through an fsynced temp file, and the previous copy is kept as `state.json.bak`.
  - Loading falls back to `.tmp` and then `.bak`. An unreadable list is set aside, never overwritten with an empty one.
  - Saves run in the background, and the speed display uses real elapsed time.
- **Windows.**
  - `.part` files are marked sparse. This stops NTFS from zero-filling the gap before the first write far into a big file, which stalled downloads and doubled disk writes. The sparse flag is cleared when the file completes.
  - The final rename retries for a few seconds when an antivirus or indexer briefly holds the file.
- **Free-space check** before starting, with a clear message, on Windows and Linux. macOS is skipped because it reports purgeable space as used.
- `Retry-After` in date form is understood. "The disk is full" is recognised.

## Not verified here

The Windows-only pieces (sparse files, `GetDiskFreeSpaceEx`, rename retries) were cross-compiled and vetted, and the API calls were reviewed. They weren't run on a Windows machine. Everything else ran on Linux under the race detector.
