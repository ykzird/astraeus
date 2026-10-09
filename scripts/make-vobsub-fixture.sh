#!/usr/bin/env bash
# Build a VobSub (dvd_subtitle) fixture out of this project's own PGS fixture.
#
# The project's notes used to say that no VobSub sample existed here and that
# ffmpeg could not make one, which is why dvd_subtitle stayed burn-only. The
# second half of that is not true: ffmpeg has no *vobsub muxer*, but it does
# have a dvdsub *encoder*, so a PGS fixture can be re-encoded into real VobSub.
# ffmpeg's own dvdsub decoder reads the result back, and burning it onto a black
# frame produces a picture tesseract reads as the caption that was drawn.
#
# The Matroska output is the useful one. VobSub stores its colours in a palette
# outside the picture stream — in a .idx sidecar, or in Matroska's codec private
# — and a bare MPEG-PS sample carries no palette at all, so an extraction has to
# keep the container that holds one. The codec private text is the same .idx
# format, which is what a decoder should read the palette from.
#
# Usage: scripts/make-vobsub-fixture.sh [output-directory]

set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$repo/internal/subtitles/testdata}"
mkdir -p "$out"

cd "$repo"

# The caption the fixture draws, and the words the OCR integration test asserts.
text="ASTRAEUS MEDIA"

echo "generating a PGS fixture for ${text@Q}"
go run ./scripts/pgsgen -text "$text" -out "$out/vobsub-caption.sup"

echo "re-encoding it as VobSub"
ffmpeg -hide_banner -loglevel error -y \
  -i "$out/vobsub-caption.sup" -c:s dvdsub "$out/vobsub-caption.mkv"
ffmpeg -hide_banner -loglevel error -y \
  -i "$out/vobsub-caption.sup" -c:s dvdsub -f dvd "$out/vobsub-caption.mpg"

# Both containers must hold what they are supposed to hold, or a decoder test
# built on them is testing the wrong thing.
for file in vobsub-caption.mkv vobsub-caption.mpg; do
  codec="$(ffprobe -v error -select_streams s -show_entries stream=codec_name -of csv=p=0 "$out/$file")"
  if [ "$codec" != "dvd_subtitle" ]; then
    echo "make-vobsub-fixture: $file holds $codec, want dvd_subtitle" >&2
    exit 1
  fi
done

# The palette lives in the container, not in the picture stream; check it, since
# a decoder that finds no palette has nothing to render. ffprobe renders the
# codec private as a hex dump whose ASCII gutter breaks a word wherever a line
# ends, so the bytes are reassembled and searched rather than grepped for.
if ! ffprobe -v error -select_streams s -show_streams -show_data "$out/vobsub-caption.mkv" 2>/dev/null \
  | python3 -c '
import re, sys

block = sys.stdin.read().split("extradata=", 1)[-1].split("extradata_size", 1)[0]
raw = bytearray()
for line in block.splitlines():
    fields = re.match(r"\s*[0-9a-f]+:\s+((?:[0-9a-f]{2,4}\s+)+)", line)
    if not fields:
        continue
    for group in fields.group(1).split():
        raw += bytes.fromhex(group)
sys.exit(0 if b"palette:" in bytes(raw) else 1)
'; then
  echo "make-vobsub-fixture: the Matroska codec private carries no palette" >&2
  exit 1
fi

ls -la "$out"/vobsub-caption.*
echo
echo "to see it rendered: ffmpeg -f lavfi -i color=black:s=1280x720:r=1:d=8 \\"
echo "  -i $out/vobsub-caption.mkv -filter_complex '[0:v][1:s]overlay' frame-%02d.png"
