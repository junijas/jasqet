#!/usr/bin/env python3
"""Builds the Firefox version of the Jasqet extension from the Chrome one."""
import json, shutil, sys, pathlib

src = pathlib.Path(__file__).parent / "extension"
dst = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else src.parent / "extension-firefox")
if dst.exists():
    shutil.rmtree(dst)
shutil.copytree(src, dst)

# UTF-8 with LF line ends on every system, like the files in extension/
def read(p): return p.read_text(encoding="utf-8")
def write(p, s): p.write_text(s, encoding="utf-8", newline="\n")

write(dst / "compat.js",
    "// Firefox: use the promise-based browser.* API under the chrome.* name\n"
    "if (typeof browser !== 'undefined' && browser.runtime) { globalThis.chrome = browser; }\n")

m = json.loads(read(src / "manifest.json"))
m["background"] = {"scripts": ["compat.js", "background.js"]}
for cs in m["content_scripts"]:
    cs["js"] = ["compat.js"] + cs["js"]
# The add-on's ID keeps the old name (TurboGet): a new ID would be a different
# add-on to Firefox, so installed copies would lose their updates and their Allow.
m["browser_specific_settings"] = {"gecko": {"id": "turboget-catcher@turboget.local", "strict_min_version": "140.0",
    "data_collection_permissions": {"required": ["none"]}}}
write(dst / "manifest.json", json.dumps(m, indent=2) + "\n")

for page in ["popup.html", "batch.html"]:
    p = dst / page
    html = read(p)
    html = html.replace("<script src=", '<script src="compat.js"></script>\n<script src=', 1)
    write(p, html)
print("Firefox extension written to", dst)
