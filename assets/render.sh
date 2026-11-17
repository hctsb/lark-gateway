#!/bin/sh
# Renders the logo with Edge (headless): logo.svg for large sizes, the simpler
# logo-small.svg for 16-48 px, then packs logo.ico from the right one per size.
EDGE="/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"
cd "$(dirname "$0")"; here=$(pwd -W)
shot() { # svg size out
  printf '<html><body style="margin:0;background:transparent"><img src="%s" width="%s" height="%s"></body></html>' "$1" "$2" "$2" > _render.html
  "$EDGE" --headless=new --disable-gpu --hide-scrollbars --default-background-color=00000000 \
    --window-size=$2,$2 --screenshot="$here/$3" "file:///$here/_render.html" >/dev/null 2>&1
}
for s in 1024 512 256; do shot logo.svg $s logo-$s.png; done
for s in 16 24 32 48; do shot logo-small.svg $s _ico-$s.png; done
shot logo.svg 256 _ico-256.png
shot logo.svg 128 _ico-128.png
python - <<'PY'
import struct
sizes = [16, 24, 32, 48, 128, 256]
pngs = [open(f"_ico-{s}.png", "rb").read() for s in sizes]
head = struct.pack("<HHH", 0, 1, len(sizes))
off = 6 + 16 * len(sizes); dirs = b""
for s, p in zip(sizes, pngs):
    dirs += struct.pack("<BBBBHHII", s % 256, s % 256, 0, 0, 1, 32, len(p), off); off += len(p)
open("logo.ico", "wb").write(head + dirs + b"".join(pngs))
PY
rm -f _render.html _ico-*.png
