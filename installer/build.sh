#!/bin/sh
# Builds Jasqet-Setup.exe. Usage: installer/build.sh [version]
set -e
cd "$(dirname "$0")/.."
V=${1:-1.6.2}
P=installer/payload
rm -rf "$P" && mkdir -p "$P"
# cgo off, like every release: with MinGW, ld would also add MinGW's own manifest
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o "$P/Jasqet.exe" .
cp icon.ico README.md LICENSE.txt "$P/"
cp -r extension "$P/extension"
python3 make_firefox.py "$P/extension-firefox" >/dev/null
makensis -V2 -DVERSION="$V" -DSRCDIR="payload" -DOUTFILE="Jasqet-Setup.exe" installer/jasqet.nsi
echo "built installer/Jasqet-Setup.exe"
