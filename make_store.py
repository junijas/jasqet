#!/usr/bin/env python3
"""Builds store-ready zips of the extension (Chrome Web Store, Edge Add-ons, Firefox AMO).
Store rules don't allow YouTube downloading inside extensions, so these editions
leave the YouTube button out; YouTube links still work when pasted into the app."""
import json, pathlib, shutil, subprocess, sys, tempfile, zipfile

root = pathlib.Path(__file__).parent
out = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else root / "store-packages")
out.mkdir(parents=True, exist_ok=True)

# UTF-8 with LF line ends on every system, like the files in extension/
def read(p): return p.read_text(encoding="utf-8")
def write(p, s): p.write_text(s, encoding="utf-8", newline="\n")

def store_edition(src: pathlib.Path, dst: pathlib.Path):
    shutil.copytree(src, dst)
    bg = dst / "background.js"
    write(bg, read(bg).replace("const YOUTUBE = true;", "const YOUTUBE = false;"))
    m = json.loads(read(dst / "manifest.json"))
    m["description"] = "Sends your browser downloads to the free Jasqet download manager and adds a Download video button to video players."
    for cs in m["content_scripts"]:
        cs["exclude_matches"] = ["*://*.youtube.com/*", "*://*.youtube-nocookie.com/*"]
    write(dst / "manifest.json", json.dumps(m, indent=2) + "\n")

def zipdir(d: pathlib.Path, z: pathlib.Path):
    with zipfile.ZipFile(z, "w", zipfile.ZIP_DEFLATED) as f:
        for p in sorted(d.rglob("*")):
            if p.is_file():
                f.write(p, p.relative_to(d))

with tempfile.TemporaryDirectory() as t:
    t = pathlib.Path(t)
    store_edition(root / "extension", t / "chrome")
    zipdir(t / "chrome", out / "jasqet-chrome-edge-store.zip")
    subprocess.run([sys.executable, str(root / "make_firefox.py"), str(t / "ff-full")], check=True, capture_output=True)
    store_edition(t / "ff-full", t / "firefox")
    zipdir(t / "firefox", out / "jasqet-firefox-amo.zip")
print("store packages written to", out)
