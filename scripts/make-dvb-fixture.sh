#!/usr/bin/env bash
# Build a DVB (dvb_subtitle) fixture out of this project's own PGS fixture.
#
# DVB subtitles were burn-only for the same reason VobSub was: no sample existed
# here and it was assumed one could not be made. The VobSub route does not
# transfer - ffmpeg's dvbsub *encoder* accepts only bitmap subtitle input, and
# refuses its own dvdsub output - but ffmpeg also has a PGS *decoder*, so a
# bitmap subtitle can be decoded and re-encoded as DVB. That is the unlock.
#
# One detail decides whether the result is readable, and it is not obvious.
# ffmpeg's dvbsub encoder authors against a 720x576 canvas and rescales whatever
# it is given to fit. Feeding it the project's 640x360 fixture therefore warps
# every glyph and the recogniser reads "RSTRAELS MEDIA". Authoring the PGS
# fixture at 720x576 - the canvas the encoder actually uses - makes the encode a
# straight copy, and tesseract reads the caption exactly.
#
# Unlike VobSub, a DVB track carries its own palette and composition in the
# stream as segments, so the container is incidental: Matroska is written
# because it is what the other readers' tests read, and MPEG-TS because that is
# what a broadcast actually uses. Both are generated so the decoder can be held
# against the transport it really arrives in.
#
# Usage: scripts/make-dvb-fixture.sh [output-directory]

set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$repo/internal/subtitles/testdata}"
mkdir -p "$out"

cd "$repo"

# The caption the fixture draws, and the words the OCR integration test asserts.
text="ASTRAEUS MEDIA"
# The canvas ffmpeg's dvbsub encoder authors against, and a caption scale that
# keeps the 5x7 glyphs legible after the encode. At the source's own scale of 4
# the glyphs come out 28 pixels tall and read exactly; larger multiples of this
# blocky font are read as heavier and go wrong.
author_width=720
author_height=576
text_scale=4

echo "generating a PGS fixture for ${text@Q} at ${author_width}x${author_height}"
go run ./scripts/pgsgen -text "$text" -text-scale "$text_scale" \
  -video-width "$author_width" -video-height "$author_height" \
  -x 170 -y 480 -out "$out/dvb-caption.sup"

echo "re-encoding it as DVB subtitles"
ffmpeg -hide_banner -loglevel error -y \
  -i "$out/dvb-caption.sup" -c:s dvbsub "$out/dvb-caption.mkv"
ffmpeg -hide_banner -loglevel error -y \
  -i "$out/dvb-caption.sup" -c:s dvbsub -f mpegts "$out/dvb-caption.ts"

# Both containers must hold what they are supposed to hold, or a decoder test
# built on them is testing the wrong thing.
for file in dvb-caption.mkv dvb-caption.ts; do
  # An MPEG-TS carries the stream under more than one program, so the probe
  # answers once per program; the first answer is the codec.
  codec="$(ffprobe -v error -select_streams s -show_entries stream=codec_name -of csv=p=0 "$out/$file" | head -1)"
  if [ "$codec" != "dvb_subtitle" ]; then
    echo "make-dvb-fixture: $file holds $codec, want dvb_subtitle" >&2
    exit 1
  fi
done

# A DVB track carries its palette in the stream rather than in the container, so
# the check that matters is not "is there extradata" but "does ffmpeg decode a
# picture out of it". The caption is rendered on the authoring canvas and read
# with tesseract, which is the same thing the integration test does.
render_dir="$(mktemp -d)"
trap 'rm -rf "$render_dir"' EXIT
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "color=black:s=${author_width}x${author_height}:r=5:d=8" \
  -i "$out/dvb-caption.mkv" \
  -filter_complex '[0:v][1:s]overlay' -frames:v 4 "$render_dir/frame-%02d.png"

# The fourth frame is inside the cue; the first is the opening black frame.
if ! python3 - "$render_dir/frame-04.png" "$text" <<'PY'
import sys
from PIL import Image
import subprocess

frame, want = sys.argv[1], sys.argv[2]
image = Image.open(frame).convert("RGB")
box = image.getbbox()
if box is None:
    sys.exit("make-dvb-fixture: ffmpeg decoded no subtitle picture")
crop = image.crop(box)
width, height = crop.size
# Dark ink on a white page, the same rendering the server's OCR path uses.
page = Image.new("L", (width + 16, height + 16), 255)
pixels, page_pixels = crop.load(), page.load()
for y in range(height):
    for x in range(width):
        r, g, b = pixels[x, y]
        page_pixels[x + 8, y + 8] = 255 - ((299 * r + 587 * g + 114 * b) // 1000)
page.save(frame + ".page.png")
read = subprocess.run(
    ["tesseract", frame + ".page.png", "stdout", "--psm", "6"],
    capture_output=True, text=True,
).stdout.strip()
if want not in read:
    sys.exit(f"make-dvb-fixture: ffmpeg's own decode reads {read!r}, want {want!r}")
print(f"  ffmpeg's decode reads {read!r}, which contains {want!r}")
PY
then
  echo "make-dvb-fixture: the fixture does not decode to the caption it drew" >&2
  exit 1
fi

ls -la "$out"/dvb-caption.*
echo
echo "to see it rendered: ffmpeg -f lavfi -i color=black:s=720x576:r=5:d=8 \\"
echo "  -i $out/dvb-caption.mkv -filter_complex '[0:v][1:s]overlay' frame-%02d.png"
