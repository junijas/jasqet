# Publishing the Jasqet extension (so the installer adds it automatically)

Browsers only let installers add extensions that are in their official stores,
and even then they ask you to switch it on once. After you publish, send the
store IDs and the installer can be rebuilt with them
(`makensis -DCHROME_STORE_ID=… -DEDGE_STORE_ID=…`).

Use the files in `store-packages/`. They leave out the YouTube button because
the stores don't allow YouTube downloading inside extensions. YouTube still
works in the Jasqet app (paste the link into Add link), and in the full
extension you load yourself.

## Chrome Web Store (one-time US$5 registration fee)
1. Go to the Chrome Web Store Developer Dashboard and sign in with a Google account.
2. Pay the one-time registration fee and verify your email.
3. **New item** → upload `jasqet-chrome-edge-store.zip`.
4. Fill in the listing (text below), add screenshots (1280×800) and the 128 px icon
   from `extension/icons/128.png`.
5. **Privacy** tab: single purpose, permission justifications and data use (below),
   and a link to the privacy policy (host `PRIVACY.md` anywhere public, e.g. a
   free GitHub page or Google Sites).
6. Submit for review (usually a few days). The item ID is in the dashboard URL.

## Microsoft Edge Add-ons (free)
1. Sign in to Microsoft Partner Center and enrol in the Edge program (free).
2. Upload the same `jasqet-chrome-edge-store.zip`, reuse the same texts.
3. Submit. The Edge product ID appears once published.

## Firefox Add-ons (free)
1. Sign in at addons.mozilla.org (Developer Hub) → **Submit a New Add-on**.
2. Choose **On this site** (listed) or **On your own** (signed file only).
3. Upload `jasqet-firefox-amo.zip`. Mozilla also asks for the source code:
   upload the source zip (everything is plain JavaScript, no build step).

## Listing text
**Name:** Jasqet Catcher
**Short description:** Sends your downloads to the free Jasqet download manager: faster, resumable, schedulable.
**Description:**
Jasqet Catcher connects your browser to Jasqet, a free, open-source download
manager for Windows. Downloads you start are handed to Jasqet, which uses many
connections at once, resumes after drops and crashes, and can schedule downloads
for cheap night data. Video players get a "Download video" button with a quality
menu. Right-click any page to pick which of its files to download, or just type
what you want ("all PDFs over 1 MB"). Magnet links open in Jasqet, and you can
take a single file out of a ZIP without downloading the whole ZIP.
Requires the Jasqet app (free). No ads, no tracking.

## Single purpose
Hand downloads, video streams and page links from the browser to the Jasqet
download manager app running on the same computer.

## Permission justifications
- **downloads**: notice downloads the user starts and hand them to Jasqet instead.
- **cookies**: pass the user's own login cookies for the file's site to Jasqet,
  so downloads that need a login work. Only for the sites being downloaded from;
  sent only to the Jasqet app on the same computer (127.0.0.1).
- **webRequest**: notice when a video player loads a video playlist so the
  "Download video" button can offer it. Read-only; nothing is blocked or changed.
- **contextMenus**: "Download with Jasqet" and "Download links on this page".
- **storage**: remember the user's settings.
- **Host permission (all sites)**: the download button and download catching must
  work on any website the user visits.

## Data use (for the store's form)
The extension collects no personal data and sends nothing to the developer or
any third party. Download links, the page address and the site's cookies are
sent only to the Jasqet app on the user's own computer, when the user
downloads something.
