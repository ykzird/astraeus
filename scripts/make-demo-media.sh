#!/usr/bin/env bash
#
# Generates a small demo library and registers it in a demo database, so the
# server and UI have real content to show without committing any media to the
# repository.
#
# Usage: scripts/make-demo-media.sh [media-dir] [db-path]
#
# Defaults: <repo-parent>/demo-media and <repo>/demo.db

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(dirname "$here")"
media_dir="${1:-$(dirname "$repo")/demo-media}"
db="${2:-$repo/demo.db}"
server="$repo/astraeus-server"

for tool in ffmpeg ffprobe; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool is required" >&2; exit 1; }
done

movies="$media_dir/movies"
shows_root="$media_dir/shows"
shows="$shows_root/Astraeus Show/Season 01"
mkdir -p "$movies/Blade Runner 2049 (2017)" "$movies/Arrival (2016)" "$shows"

# clip <path> [seconds] renders a short H.264/AAC test clip.
clip() {
  local path="$1" seconds="${2:-3}"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc=size=640x360:rate=15:duration=$seconds" \
    -f lavfi -i "sine=frequency=440:duration=$seconds" \
    -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest \
    "$path"
  echo "  $(basename "$path")"
}

# captioned_clip <path> muxes a real subrip track so subtitles can be exercised.
captioned_clip() {
  local path="$1" srt
  srt="$(mktemp --suffix=.srt)"
  cat >"$srt" <<'SUBS'
1
00:00:00,500 --> 00:00:02,000
Astraeus brings the stars indoors.

2
00:00:02,100 --> 00:00:02,900
Second cue, with "quotes" & ampersands.
SUBS

  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc=size=640x360:rate=15:duration=3" \
    -f lavfi -i "sine=frequency=440:duration=3" \
    -i "$srt" \
    -map 0:v -map 1:a -map 2:s \
    -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -c:s srt \
    -shortest "$path"
  rm -f "$srt"
  echo "  $(basename "$path")  (with a subrip track)"
}

# dual_audio_clip <path> renders a clip with two audio tracks that differ in both
# language and channel count, so the audio menu has something to choose between
# and the choice is visible in what arrives: track 1 is English stereo, track 2 is
# German mono and is the track the file marks default. Both are AAC, so a browser
# takes either as it is.
#
# The picture is HEVC, which no browser here decodes, so the server transcodes
# this clip: that is what makes the quality menu appear alongside the audio one,
# and it is the combination the interface checks need.
dual_audio_clip() {
  local path="$1"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=15:duration=4" \
    -f lavfi -i "sine=frequency=440:duration=4" \
    -f lavfi -i "sine=frequency=880:duration=4" \
    -map 0:v -map 1:a -map 2:a \
    -c:v libx265 -preset ultrafast -crf 30 -pix_fmt yuv420p -c:a aac -b:a 96k \
    -ac:a:0 2 -ac:a:1 1 \
    -metadata:s:a:0 language=eng -metadata:s:a:0 title="English stereo" \
    -metadata:s:a:1 language=deu -metadata:s:a:1 title="Deutsch mono" \
    -disposition:a:0 0 -disposition:a:1 default \
    -shortest "$path"
  echo "  $(basename "$path")  (HEVC, two audio tracks, second is default)"
}

echo "Generating demo media in $media_dir"
clip "$movies/Blade Runner 2049 (2017)/Blade Runner 2049 (2017).mp4"
clip "$shows/Astraeus Show S01E01 - Pilot.mkv"
clip "$shows/Astraeus Show S01E02 - Descent.mp4"
captioned_clip "$shows/Astraeus Show S01E03 - Captions.mkv"
dual_audio_clip "$movies/Arrival (2016)/Arrival (2016).mp4"

if [ ! -x "$server" ]; then
  echo
  echo "The server binary is missing, so the libraries were not registered."
  echo "Build it, then run these:"
  echo "  go build -o astraeus-server ./cmd/astraeus-server"
  echo "  ./astraeus-server scan --db $db --path $movies --kind movies --name 'Demo Movies'"
  echo "  ./astraeus-server scan --db $db --path $shows_root --kind shows --name 'Demo Shows'"
  exit 0
fi

echo
echo "Registering and scanning into $db"
"$server" scan --db "$db" --path "$movies" --kind movies --name "Demo Movies"
"$server" scan --db "$db" --path "$shows_root" --kind shows --name "Demo Shows"
"$server" enrich --db "$db"

echo
echo "Serve it with:"
echo "  ./astraeus-server serve --db $db --web-dir web"
