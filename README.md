# Jasqet: your own download manager

<p align="center"><img src="docs/jasqet-logo.png" alt="Jasqet" width="300"></p>

Jasqet downloads files over many connections at once (up to 32), the same trick
IDM uses. It pauses and resumes, survives crashes and power cuts, catches
downloads from your browser with your login cookies, saves videos from web
players and **YouTube** (whole videos, playlists, or just the part you need),
downloads **torrents**, has a **scheduler** so big downloads can run at dawn
when data is cheaper or faster, and a **built-in viewer** for videos, music,
pictures and PDFs, even while they're still downloading.

It's free and open source (MIT licence): no ads, no bundled software, no tracking.

**[Download Jasqet](https://github.com/junijas/jasqet/releases/latest)** for
Windows 10 and 11. There are also builds for Windows on ARM, Linux and Mac.

| File on the Releases page | For |
|---|---|
| `Jasqet-Setup.exe` | Windows 10 or 11 (64-bit), with the installer. Start here. |
| `Jasqet-windows-x64.zip` | The same app without the installer: unzip it anywhere and run `Jasqet.exe`. |
| `Jasqet-windows-arm64.zip` | Windows on ARM (Surface Pro X, Snapdragon laptops). |
| `Jasqet-linux-x64.zip` | Linux (64-bit). Opens in your browser as an app window. |
| `Jasqet-mac-apple.zip`, `Jasqet-mac-intel.zip` | Mac with Apple chips or Intel. Opens in your browser as an app window. |
| `SHA256SUMS.txt` | Fingerprints of every file, to check your download. |

## What it does

- **Fast, unbreakable downloads:** up to 32 connections per file, tuned by
  itself. Pause and resume, carry on after a crash or a power cut, and try
  again by itself when the internet drops. Expired links are found again on
  the page they came from.
- **Your browser hands its downloads over** (Chrome, Edge, Brave, Opera,
  Vivaldi and Firefox), with your login, so files behind a sign-in work too.
- **Videos and YouTube:** a **Download video** button on video players, whole
  playlists, only the part of a video you need, subtitles, and audio only.
- **Torrents and magnet links**, with a check for fake torrents.
- **Save data:** a scheduler for night bundles, a data budget it never goes
  over, a pause on metered connections, and copies from other computers on your
  Wi-Fi instead of the internet.
- **A built-in viewer** for videos, music, pictures and PDFs, even while they
  are still downloading.
- **Safety checks after every download:** is the file really what its name
  says, checksums, Windows Defender, and VirusTotal if you add a key.
- **Automation:** follow YouTube channels, podcasts, GitHub releases or any
  page, and rules for after a download (unzip, rename, move).

The full guide follows.

## 1. Install (Windows 10 or 11)

1. Run **Jasqet-Setup.exe**. Windows may say "Windows protected your PC"
   because the app isn't signed by a paid certificate: click **More info → Run
   anyway**. No administrator rights are needed.
2. Leave **Start with Windows** ticked if you use the scheduler (Jasqet then
   waits quietly in the tray, by the clock).
3. At the end, tick **Connect my browser**, and Jasqet shows you how (next part).

Jasqet opens in its own window and keeps running in the **tray** (the small
icons by the clock) when you close it. Right-click the tray icon for Pause all,
Resume all, Scheduler on/off, Quiet mode and Quit.

Downloads go to `Downloads\Jasqet`, sorted into Video, Music, Compressed,
Programs, Documents… (change this in Settings ⚙).

Prefer no installer? The zip has `Jasqet.exe`, which runs from any folder.

## 2. Connect your browser (once)

Browsers don't let any program install extensions by itself, so this takes a
minute. In Jasqet click **Connect** on the banner (or Settings → General →
Connect my browser…), then:

1. Click your browser. Its extensions page opens.
2. Turn on **Developer mode** (top right in Chrome and Edge).
3. Click **Load unpacked** and pick the folder Jasqet shows you
   (`extension`, in Jasqet's folder).
4. Pin the blue Jasqet icon. The window says **Connected** when it works.

Jasqet only listens to its own extension. The one in Jasqet's folder is
recognised by itself; anything else that asks (Firefox, or the extension loaded
from another folder) shows **"A browser extension … wants to send downloads to
Jasqet"** in the window. Click **Allow** once. Other extensions you have
installed can't use Jasqet, and even Jasqet's extension can only hand over
downloads: settings, opening files and deleting stay in Jasqet's window.

The extension also checks it's talking to the real Jasqet before it sends
your cookies or links. The first time, Jasqet gives it a secret (they
**pair**), and after that Jasqet has to prove it has that secret. If another
program sits on Jasqet's address while Jasqet is closed, it gets nothing, and
the browser keeps its own download. If you remove the extension and add it
back (or clear its data), Jasqet asks in its window: "Jasqet's extension in
Edge wants to pair again". Click **Pair**, and the browser pairs by itself in a
few seconds. If the Jasqet icon in your browser says **Not paired** for
another reason, click **Pair again** there. Pair only while Jasqet is open: if
it isn't, another program may be answering in its place. The extension needs a
Jasqet that knows pairing: with an older Jasqet it sends nothing until you
update Jasqet.

**Firefox (140 or newer):** open `about:debugging` → **This Firefox** → **Load
Temporary Add-on** → pick `manifest.json` in the `extension-firefox` folder,
then click **Allow** in Jasqet.
Firefox forgets temporary add-ons when it restarts; to keep it, zip that
folder's contents and get it signed free at addons.mozilla.org ("On your own").

**Updating from an older Jasqet?** Click the reload ↻ button on Jasqet
Catcher in `chrome://extensions`, then reload any open pages.

**Used TurboGet?** Jasqet is its new name (from 1.6.1). The installer goes
over TurboGet in the same folder, so the extension you already loaded keeps
working (reload it as above to see the new name). Your download list and
settings move from `%AppData%\TurboGet` to `%AppData%\Jasqet` the first time
Jasqet starts, and your downloads stay where they are (`Downloads\TurboGet`,
unless you change it in Settings).

(Once the extension is published in the Chrome Web Store and Edge Add-ons, the
installer can make the browsers offer it by themselves. See
`docs/STORE-PUBLISHING.md`.)

## 3. Everyday use

- **Add link** (or `Ctrl+V` anywhere in the window with a link copied, or drag a
  link onto the window). Paste several links, one per line.
- **Numbered files:** `https://site.com/lecture[01-12].mp4` adds lecture01 … lecture12.
- Downloads you start in the browser come to Jasqet by themselves.
  Right-click any link → **Download with Jasqet**.
- **Google Drive and Dropbox share links** work directly. Paste them as they are.
- **Grab files from a page:** right-click the page → **Download links on this
  page with Jasqet…**. Type what you want in plain words, like *"all PDFs over
  1 MB"*, *"videos with lecture in the name"*, *"first 10 mp3s"* or *"zip files
  except draft"*, and Jasqet ticks them (it checks sizes using almost no
  data). It follows simple rules, so it shows what it understood.
- Click a download to see details. You can change its connections and speed
  limit while it runs.
- **Cancel (✕)** stops a download but keeps it in the list, with what it already
  got. Press ▶ to continue later, or the bin to remove it.
- **FTP** links, **sites that need a login** (Add link → Login) and **proxies**
  (HTTP or SOCKS, Settings → Network) all work.

## 4. The built-in viewer

**Open** (the button on a finished download, a double-click on its row, or
**Open** on the "download finished" notification) shows videos, music,
pictures and PDFs right inside the Jasqet window; **← Downloads** (or `Esc`)
takes you back to the list. Everything else (Word, Excel, ZIP…) opens in your
computer's normal program. Prefer a separate window? Settings → Viewer →
**Show files: In a window of their own**.

- **Videos:** MP4, MKV, WebM, MOV, M4V, plus TS and FLV (re-wrapped as MP4 in a
  moment, without re-encoding). Play/pause, a seek bar with thumbnails,
  volume, speed 0.5× to 2×, sound-track and subtitle pickers, full screen.
  Keys: `Space` play/pause, `←` `→` 5 s, `↑` `↓` volume, `F` full screen,
  `M` mute, `Esc` close.
- **Subtitles:** a `.srt`, `.vtt`, `.ass` or `.ssa` file with the same name next
  to the video is switched on by itself; text subtitles inside MKV files are in
  the picker.
- **Music:** MP3, M4A/AAC, FLAC, WAV, OGG, Opus, with the cover art, title and
  artist from the file.
- **Pictures:** JPG, PNG, GIF (animated), WebP, BMP, SVG, AVIF. Zoom (wheel),
  fit, actual size, rotate, `←` `→` for the next picture.
- **PDFs** (read-only): page thumbnails, go to page, zoom, fit width, find
  text, select and copy text.
- The top bar shows the name, size and site, with **Open externally**, **Show
  in folder** and **Delete**; ‹ › step through the other files in the same
  folder. Reopening a video carries on where you stopped (and a separate
  viewer window remembers its size and place).
- **Right-click any download** for Open, Open externally, **Open with…**
  (choose another app), Show in folder.

**Preview while downloading:** downloads in progress have a **Preview** (eye)
button. While you watch, the connections line up one after another right after
the part that's playing, instead of each working on its own far-off piece, so
even a server that slows every connection down feeds the player with all of
them. Each asks for about a second's worth at a time and the next starts where
that answer ends, so the connections stay open instead of reconnecting.
Wherever the player needs something that hasn't arrived (an MP4's index
at the end of the file, or a jump ahead) a connection starts at exactly that
byte; if the connection there goes silent, another takes over within seconds.
Video-site downloads play their separate video and sound together until
they're joined, and torrents switch to in-order pieces. When playback catches
up with the download it says **"Waiting for download: 37%"** and carries on by
itself. Close the viewer and the download goes back to normal. Preview only
reads the file; checks and checksums still run at the end.

**Safety** (these are rules, not options):

- Every Open checks what the file **really is**. A "video", "song" or "PDF"
  that's really a program, script, archive or web page is blocked, with
  **Delete** and **Show in folder**.
- Open **never runs a program**. A real `setup.exe` asks first, in plain words,
  and only runs when you click **Run it** in Jasqet's own window.
- PDFs open **locked down**: no scripts, forms can't be filled or sent,
  attachments aren't opened, and a link shows you its **full address** and
  asks before opening in your browser.
- SVG pictures can't run scripts or load anything from the internet.
- The viewer runs **separately from your downloads**: inside the window it
  lives in a process of its own (it's loaded from the app's second local
  address, so the browser engine keeps them apart), so a broken file can't
  stop your downloads or the list. If a file makes it stop responding you get
  "This file could not be displayed" with Open externally. Damaged files say so and offer **Verify**
  (checksum, plus 8 small pieces compared with the website, about 256 KB in all) and
  **Re-download**.

**Settings → Viewer:** Open with the viewer, your default program, or ask each
time; inside the Jasqet window or in a window of their own; a different choice per type (videos, music, pictures, PDFs); open
automatically when a download finishes (off at first); preview while
downloading; hardware video decoding; matching subtitle files; remembering
where you stopped.

**What it doesn't do** (it tells you, and Open externally is one click away):
old AVI/DivX/Xvid videos, Dolby AC-3/DTS sound, HEIC photos, picture
subtitles from DVDs/Blu-rays, subtitles stored inside MP4 files, and
copy-protected files. MKVs play when Edge's player knows what's inside (it
does for almost all of them). PDFs open once they're fully downloaded, so early
pages of "fast web view" PDFs aren't shown during the download.

## 5. Save data and money

- **Data budget** (Settings → Data saver): "I have 3 GB left". Jasqet never
  goes over it: downloads that won't fit wait, and everything stops just
  before the limit. It can start again on your bundle's renewal day. The amount
  left shows at the top of the window.
- **Scheduler** (clock button): match your night bundle (e.g. 00:05 to 04:55).
  Add **more time windows** for other bundles (e.g. a weekend one). At the stop
  time Jasqet pauses and carries on in the next window. Optionally sleep or
  shut down when done. Add downloads with **Download later** and they wait.
- **Learns when your internet is fastest:** while it downloads, Jasqet notes
  the best speed of each hour of the day. After a few days the Scheduler shows
  it as a chart, and when one stretch is clearly faster a banner says so (e.g.
  "usually 3× faster from 1 AM to 5 AM") with a button to start scheduled
  downloads then. Only times without a speed limit count; it's 24 numbers kept
  on your computer.
- **Qualities that fit your data:** with a data budget, video qualities bigger
  than the data you have left are marked (in the Add dialog and the browser's
  video menu), and the best one that fits is picked for you.
- **Metered connections:** on a phone hotspot or capped Wi-Fi (Windows knows,
  and Android hotspots say so), Jasqet pauses (or downloads slowly, your
  choice) and shows a **Download anyway** button.
- **Files you already have:** before downloading, Jasqet compares a few
  small pieces of the file (about 200 KB) with files of the same size in your
  folders. Same file → it tells you instead of downloading it again.
- **Sharing on your network:** turn on Settings → Network → Share downloads. If
  another computer with Jasqet on the same Wi-Fi already has the file, it's
  copied from there with no internet data. Only computers with the same
  group code share. Jasqet makes one when you turn sharing on (16 letters and
  digits, like `KQ7M 4TZP 9XWD 2HRC`): click **Show code** to see or copy it,
  and type it on your other computers. A code you make up needs at least 12
  letters or digits. Without a code nothing is shared or copied, and the code
  itself never travels over the network. To ask for a file, Jasqet sends a
  scrambled fingerprint of its link and name that only computers with your
  code can match with files they have, never the link or name themselves. It
  asks them all at once and waits 4 seconds at most, so a slow computer can't
  hold up a download. Files you downloaded with a login or your cookies, or
  from a link with a password or a `?` in it (often the key to the file), are
  never shared. A copy is only kept if pieces taken at random places match the
  file on the website, and the whole file its checksum when one is known;
  otherwise it's downloaded from the internet. Videos and other files that
  can't be checked against the website are copied only because the other
  computer has your group code. Older versions can't share with this one, so
  update every computer.

## 6. Downloads that never die

- **Tries again by itself.** If the internet goes away for a while or the
  server is down, a download doesn't stop with an error: it waits and tries
  again on its own, after 5 s, 10 s, 20 s… then every 5 minutes, for about
  two and a half hours (longer while it keeps getting somewhere), and at once
  when your connection changes. The list shows *Trying again by itself in…*.
  Handy for downloads left running at dawn. (Servers that can't resume are
  tried three times only, as each try starts from the beginning.)
- **No connection holds up the end.** When one connection crawls far behind
  the others or goes silent, a fresh one takes over its part, and near the end
  free connections share the slow pieces, so the last few percent don't drag.
- **Power cuts:** progress is saved only for data that has really reached the
  disk, so a resumed file never has a hole in it. If the download list itself
  was being saved at that moment, Jasqet uses the copy saved just before.
- **Disk space** is checked before a download starts, instead of failing at
  95% with the data already spent.

- **Expired links are refreshed automatically.** Jasqet remembers the page a
  link came from. When a link stops working (common with file hosts, CDN links
  and Google Drive), it finds the same file on that page again (same name,
  same size) and carries on without asking. If it can't, use **Refresh link**.
- **Nothing broken gets in.** When a cut-off piece is picked up again, Jasqet
  re-reads a little of what it already has and compares it; if the file
  changed on the server meanwhile, it starts that file again instead of making
  a broken mix. Video chunks that arrive as an error page are thrown away, and
  only that chunk is fetched again. Torrents check every piece.
- **Connections tune themselves:** Jasqet starts with 8, adds more while
  that makes it faster, gives them back when it doesn't, and halves when a
  server says "too many" (turn off in Settings → General).
- **Switching Wi-Fi ⇄ mobile data:** Jasqet notices the new connection and
  reconnects at once, keeping everything downloaded.

## 7. Download smarter

- **One file out of a ZIP:** for a `.zip` link, click **Look inside the ZIP
  first** (or right-click the link → Look inside this ZIP with Jasqet). Tick
  the files you want; only their bytes are downloaded, then checked. A 4 GB ZIP
  with one 20 MB file you need costs about 20 MB.
- **Several links to the same file (mirrors):** paste them all, tick **These
  links are copies of the same file**. Every connection takes pieces from
  whichever server is free. Each mirror is checked against the first link and
  dropped if its file differs; a server that dies is simply left out.
- **Torrents and magnet links:** paste a magnet link, a `.torrent` link, or
  open a `.torrent` file; clicked magnet links in the browser come to Jasqet.
  Pick which files you want. Web servers listed in the torrent (web seeds), or
  mirror links you add, help when few people are sharing. Jasqet stops
  sharing back once a torrent finishes, to save your data (changeable in
  Settings → Network, with an upload speed limit). If Windows asks whether
  Jasqet can use the network the first time, allow it.
- **Only part of a video:** when choosing a quality, fill **From** and **To**
  (e.g. 2:30 to 5:00). Only the chunks for that part are downloaded, and the size
  estimate shows the saving. The clip starts at the nearest clean picture
  before your start time (usually under 5 seconds early).
- **Subtitles:** for YouTube, pick a language (including automatic captions);
  they're saved as an `.srt` next to the video, lined up with clips too.
- **Automatic subtitles for any video or recording:** right-click a finished
  video, lecture or podcast → **Make subtitles**, or **Make English subtitles**
  to translate. Whisper listens on your computer (nothing is uploaded) and
  saves "Name.English (auto-generated).srt" next to the file; the viewer shows
  it. The first time, it downloads its speech engine and language model (about
  87 MB, once, checked against fixed checksums). It works out the language from
  samples across the whole recording, so an accent or a jingle at the start
  doesn't fool it. On this PC it listened about 9× faster than real time.
  Good in English and many big languages; weak in Twi and other languages it
  hasn't heard much. On a Mac or Linux, install whisper.cpp first.
- **Tidy video names:** "[YTS.MX] Some.Movie.2021.1080p.WEBRip.x264.mp4" is
  saved as "Some Movie (2021).mp4", and TV episodes like
  "The.Office.US.S02E05.720p.WEB-DL.mkv" as "The Office US - S02E05.mkv" in a
  folder for the series. Names that aren't from release sites stay as they
  are. Settings → General → Tidy video names.
- **Smooth mode** (Settings → General): downloads at full speed, but eases off
  the moment your browsing, WhatsApp calls or games would start to lag (it
  watches your ping), then speeds up again.

## 8. Safety

After each download, Jasqet checks:

- **Is it really what its name says?** A "movie.mp4" that is actually a
  program, or "song.mp3.exe", is renamed to `….blocked` so it can't be opened
  by mistake, with a warning (you can restore the name). A download that's
  really a web page (login page, "link expired") is flagged.
- **Checksums:** Jasqet looks for the SHA-256 / SHA-1 / MD5 the site
  publishes (`file.sha256`, `SHA256SUMS`, `checksums.txt`, or on the download
  page) and compares. You can also paste one (Add link → Checksum, or **Check a
  checksum** on a finished download).
- **Windows Defender** scans programs, archives and documents (and other files
  under 300 MB). Files are marked as coming from the internet, like browsers
  do, so SmartScreen and Office Protected View check them too. Add a free
  **VirusTotal** key (Settings → Safety) to look files up there. Only the
  file's fingerprint is sent, never the file.
- **Fake torrents:** before anything downloads, the file list is checked for
  the usual tricks: "codec" or "player" programs in a movie, shortcuts and
  scripts, password files, locked archives where the video should be. A risky
  torrent waits for you; untick the bad files or download anyway.

## 9. Automation

- **Watch rules** (eye button): follow a **YouTube channel or playlist**, a
  **podcast feed**, a **GitHub project's releases** or **any web page**.
  Jasqet checks every few hours and downloads what's new, at your scheduled
  time if you like. The first check just remembers what's already there.
- **After downloading** (Settings → After downloading): rules like "unzip every
  archive and delete the zip", "save the sound of lecture videos as .m4a",
  "rename to {date} {name}", "move PDFs to D:\Books". ZIP, RAR, 7z and tar.gz
  are unpacked without extra software. Files that fail a safety check are left
  alone.
- **Notifications:** a Windows notification, or a **Telegram** message on your
  phone (Settings → Notifications walks you through making a bot). Handy for
  downloads that run at dawn.

## 10. Saving videos and YouTube

**On YouTube** (videos, Shorts, playlists) and most other video websites:
1. Move the mouse over the video (or just let it play). A blue **Download
   video** button appears. Click it and the usual qualities are there at
   once; sizes, subtitles and the playlist fill in a few seconds later.
2. Pick a quality: 1080p, 720p… or **Audio only** (an .m4a, great for lectures
   and sermons). On a playlist page you can take the **whole playlist**.
   Choose **Subtitles** in the same menu to get an `.srt` with the video, or
   **Only the subtitles** if that's all you need. Your language is remembered.
3. The first time, Jasqet sets up YouTube support by itself: it downloads
   two free helper tools (yt-dlp and Deno, about 60 MB, once), the menu shows
   the progress and then the qualities. It keeps yt-dlp updated.

Videos are saved as normal **.mp4** files that play on phones, TVs and WhatsApp.
You can also paste a YouTube link (or a `.m3u8` / `.mpd` link) into Add link.

**"Confirm you're not a robot":** YouTube sometimes asks this of a whole
internet address. Mobile networks share one address between many people, so
it comes and goes, whatever app you use. Jasqet first tries YouTube's TV and
Android apps, which are often let through. If YouTube still asks, the menu
offers **Use my YouTube session**: Jasqet then uses this browser's YouTube
sign-in, only when YouTube asks, so it works for videos you paste into the
app and for dawn downloads too. On Windows it's saved encrypted, so only your
Windows account can read it (on Mac and Linux, in a file only your account can
open); forget it any time in Settings → General (or untick it in the
extension's popup). The same goes for the site logins and cookies Jasqet
keeps with your downloads, the Telegram and VirusTotal keys and a proxy
password. A login is only ever sent to the site it was given for.
YouTube may limit accounts that are used a lot with download tools, so a spare
Google account signed in to YouTube in your browser is the safest choice.

**What it can't save:** DRM-protected services (Netflix, Showmax, Prime Video,
Disney+…), live streams that are still going, and videos only for members.

YouTube's terms only allow downloading through YouTube's own app, so whether to
download YouTube videos for personal use is your call.

## Honest notes

- **Speed:** Jasqet can't go faster than your internet plan. Many connections
  help when a server limits each one (most file hosts and CDNs do): against a
  server limiting each connection to 128 KB/s, auto mode went 8 → 12 → 16
  connections and finished 30 MB in about 25 s.
- **What was tested end to end** (on test servers): expiring links refreshed
  from their page, files changing on the server (with and without version
  tags), CDN servers that label the same file differently, video chunks that
  arrive as error pages, servers answering "too many requests", mirrors
  (including one with a different file and one that dies), checksums found
  next to the file and on the page, the file-type checks, taking files out of
  a remote ZIP, clips from HLS, DASH and YouTube-style files (a frame at 1 s
  into the clip matched the source exactly), torrents from a local seeder and
  from a web seed alone (including resuming after a hard kill), fake-torrent
  detection, watch rules for pages and podcasts, unzip / extract sound /
  rename / move rules, the browser extension's magnet catching and plain-words
  link picker, plus everything from earlier versions.
- **Engine (1.6, from the 1.5.0 engine review), on a test bench that times
  real downloads against awkward servers and checks every byte:** one
  connection crawling at 24 KB/s (85 s before, 4 s now); the internet dropping
  for 100 s (used to give up, now finishes by itself); 30 small files behind a
  redirect (9.3 s and 33 connections before, 5.8 s and 3 now); a speed limit no
  longer wastes data (8.0 MB used for an 8 MB file, was 11.3 MB).
- **Preview (1.6)**, against a server that allows 25 KB/s per connection, 16
  connections, Preview pressed at 20%: an MP4 with its index at the start
  played in about 1 s, one with its index at the end in about 3 s, and a jump
  to 80 s of a 2-minute video (at about 35% downloaded) played in about 6 s;
  every time the finished file matched the original byte for byte. (The 1.5.0
  preview needed ~73 s in this test, because it fed the player with one
  connection.) 1.6.1, same test on Windows in the viewer page: a 150 s film
  with a 190 KB index at the end played after 3.1 s (7.0 s in 1.6.0), about
  as fast as one with its index at the start (2 to 3 s). Watching a 60 MB film over a link with 150 ms delay opened 17
  connections (cutting requests short would have opened 421); a connection
  going silent at the playing point held the player up 3.9 s, not 25 s.
- **Viewer (1.4), tested in Chromium (the engine inside Edge and WebView2):**
  every acceptance check in the spec: MP4, MKV (two sound tracks, inner and
  outside subtitles), WebM, MP3 and FLAC with covers, JPG, PNG, GIF, WebP, BMP,
  SVG and PDF in the viewer; Open externally and Open with…; Default program
  and per-type settings (the viewer never appears); an .exe renamed .mp4 and a
  script renamed .pdf blocked and never run; a PDF with JavaScript (no script
  ran, no form was sent); an SVG with a script and an outside picture (neither
  ran); a damaged video (error with Open externally while another download kept
  going); an MP4 with its index at the end, previewed at about 20 % from a slow
  server (it played from the start, waited at the downloaded edge and carried
  on by itself, and the finished file matched the original byte for byte);
  .docx and .zip in the default program; reopening a video carried on from the
  same second, both inside the Jasqet window and in a separate window. A
  viewer frozen on purpose left the Jasqet window working and showed "This
  file could not be displayed". Also previews of a torrent from a local seeder and of a DASH
  video with separate sound (in sync, then switched to the joined file), TS
  and FLV re-wrapping, and the notification links under Wine. The test
  browser has no H.264/AAC decoder (Edge does), so those were tested as
  re-wrapped files, not played.
- **The viewer engine:** the spec suggested mpv and PDFium. Jasqet uses the
  player built into Edge/WebView2 (already on every Windows 10/11 PC, hardware
  decoding, the same decoders as Edge) and PDF.js for PDFs, which the spec
  allows for web-based apps. That keeps Jasqet one small file and keeps the
  decoders in Edge's sandboxed processes.
- **Built for Windows but only runnable on Windows itself** (so not tested
  here): the Windows Defender scan, the "from the internet" mark, Windows
  notifications (and their Open buttons), metered-network detection, the
  window and tray, and the Windows-only parts of the viewer (full screen
  filling the whole screen, the separate window's size memory, the window's
  own crash watch). Under Wine they fall back to an Edge window as designed. YouTube
  watch rules and subtitles use the same YouTube code as before, which can't be
  fully run from a test server (YouTube blocks them). Telegram and VirusTotal
  need your own key or bot, so they were built to their published APIs but not
  sent real messages. Smooth mode needs a real home connection to measure.
- If something unusual comes along when joining a video, Jasqet can use
  `ffmpeg.exe` placed next to Jasqet.exe; without it you still get a playable
  file.

## Files
- `Jasqet.exe`: the app (Windows 64-bit). ARM, Mac and Linux builds are on
  the Releases page.
- `extension/`: the extension for Chrome, Edge, Brave, Opera and Vivaldi.
- `extension-firefox/`: the same extension for Firefox.
- Settings and the download list: `%AppData%\Jasqet\state.json`; YouTube
  helper tools: `%AppData%\Jasqet\tools`; saved torrents:
  `%AppData%\Jasqet\torrents`; where you stopped in videos:
  `%AppData%\Jasqet\viewer-positions.json`.

## Building from source
Install Go 1.26+ and run, in this folder:
`go build -ldflags "-H windowsgui" -o Jasqet.exe .`
Releases are built with cgo off. If a C compiler such as MinGW is installed,
set `CGO_ENABLED=0` first, as `installer/build.sh` does, or MinGW's own
manifest goes in next to Jasqet's and ld warns "multiple non-default manifests".
Build the Firefox extension with `python make_firefox.py`, the store packages
with `python make_store.py`, and the installer with `installer/build.sh`
(needs NSIS). With the NSIS download for Windows, which has only 32-bit stubs,
add `-DTARGET=x86-unicode` to the `makensis` line.

The other builds come from the same code:

    CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-s -w -H windowsgui" -o Jasqet.exe .
    CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o jasqet .
    CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o Jasqet-mac-apple .

Put the `extension` folder (and `extension-firefox`) next to the program: the
app looks for it there when you connect your browser.

**Tests:** `go test ./...` runs them all (about a minute). Every fix comes
with a test. A few tests only run on Windows.

**Where things are:**

| Files | What they do |
|---|---|
| `engine.go`, `resilience.go`, `robust.go`, `mirrors.go` | The download engine: connections, resuming, retries, mirrors |
| `manager.go`, `scheduler.go`, `datasaver.go`, `learn.go` | The download list, the scheduler, the data budget |
| `server.go`, `apikey.go`, `extauth.go` | The local server the window and the extension talk to, and who may use it |
| `yt.go`, `ytsession.go`, `tools.go` | Videos through yt-dlp, set up on first use |
| `media_manifest.go`, `mediajob.go`, `mediaprobe.go`, `remux.go`, `clip.go` | HLS and DASH videos, joining and clips |
| `torrent.go`, `zipremote.go`, `ftp.go`, `lan.go` | Torrents, files out of remote ZIPs, FTP, sharing on your network |
| `web/`, `desktop_windows.go` | The window itself (a web page in WebView2 on Windows) |
| `viewer.go`, `preview.go` | The viewer, and preview while downloading |
| `safety.go`, `secrets.go`, `postprocess.go` | Safety checks, saved secrets, after-download rules |
| `extension/` | The browser extension (plain JavaScript, no build step) |
| `installer/` | The NSIS installer |

## Contributing
Bug reports and pull requests are welcome. Please keep to the style of the
code around your change (short comments in plain English), add no new
dependencies without asking first, and add a test for every fix.

## Licence
MIT, see `LICENSE.txt`. PDF.js in `web/pdfjs` keeps its own licence (Apache 2.0).
What Jasqet stores and sends is in `docs/PRIVACY.md`.
