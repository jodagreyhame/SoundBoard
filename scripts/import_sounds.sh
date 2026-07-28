#!/usr/bin/env bash
#
# import_sounds.sh — normalise audio you already have into the SoundBoard library.
#
# SoundBoard ships no audio. This takes clips you own (or are otherwise licensed to
# use), levels them so they all play at a consistent volume, converts them to the
# format the engine decodes fastest, and files them under sounds/<category>/.
#
# It downloads nothing and contacts no website. Where your audio comes from, and
# whether you may use it, is your call.
#
# USAGE
#   scripts/import_sounds.sh <category> <file-or-directory>...
#   scripts/import_sounds.sh memes ~/clips/airhorn.mp3
#   scripts/import_sounds.sh games ~/clips/game-sfx/          # whole folder
#   scripts/import_sounds.sh --dry-run memes ~/clips/         # show, do not write
#
# OPTIONS
#   -n, --dry-run      print what would be written, change nothing
#   -f, --force        overwrite an existing clip of the same name
#   -q, --quiet        suppress per-file progress
#   -h, --help         this text
#
# WHAT IT DOES TO EACH FILE
#   - loudness-normalises to -16 LUFS (EBU R128, ffmpeg loudnorm) so no clip is
#     dramatically louder than the rest of your board
#   - resamples to 48 kHz stereo, which is what the audio engine mixes at; doing it
#     here means no resampling cost on first play
#   - writes .wav, the cheapest format to decode
#   - renames to snake_case: "Air Horn (loud).mp3" -> "air_horn_loud.wav"
#
# REQUIRES
#   ffmpeg on PATH.  https://ffmpeg.org/download.html
#
set -uo pipefail

TARGET_LUFS="-16"       # EBU R128 programme loudness
TARGET_TP="-1.5"        # true-peak ceiling, dBTP
TARGET_LRA="11"         # loudness range
SAMPLE_RATE="48000"
CHANNELS="2"

DRY_RUN=0; FORCE=0; QUIET=0

die()  { printf 'import_sounds: %s\n' "$*" >&2; exit 1; }
note() { [ "$QUIET" -eq 1 ] || printf '%s\n' "$*"; }
usage() { sed -n '2,/^set -uo/p' "$0" | sed 's/^# \{0,1\}//; $d'; exit "${1:-0}"; }

args=()
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--dry-run) DRY_RUN=1; shift;;
    -f|--force)   FORCE=1; shift;;
    -q|--quiet)   QUIET=1; shift;;
    -h|--help)    usage 0;;
    -*)           die "unknown option: $1 (try --help)";;
    *)            args+=("$1"); shift;;
  esac
done

[ "${#args[@]}" -ge 2 ] || usage 1
CATEGORY="${args[0]}"
SOURCES=("${args[@]:1}")

command -v ffmpeg >/dev/null 2>&1 || die "ffmpeg not found on PATH. See https://ffmpeg.org/download.html"

# Category becomes a directory name and a UI label; keep it filesystem-safe.
case "$CATEGORY" in
  *[/\\]*|.|..|"") die "invalid category: '$CATEGORY'";;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/clip_dir.sh
. "$SCRIPT_DIR/clip_dir.sh"
CLIP_DIR="$(soundboard_clip_dir)"
DEST="$CLIP_DIR/$CATEGORY"

# snake_case a display name: strip extension, lower, non-alnum -> _, squeeze, trim.
slug() {
  printf '%s' "${1%.*}" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -e 's/[^a-z0-9]\+/_/g' -e 's/_\+/_/g' -e 's/^_//' -e 's/_$//'
}

is_audio() {
  case "$(printf '%s' "${1##*.}" | tr '[:upper:]' '[:lower:]')" in
    wav|mp3|flac|ogg|oga|m4a|aac|opus|wma|aiff|aif) return 0;;
    *) return 1;;
  esac
}

# Collect inputs: files as-is, directories one level deep (categories are flat).
FILES=()
for src in "${SOURCES[@]}"; do
  if [ -d "$src" ]; then
    while IFS= read -r f; do FILES+=("$f"); done < <(find "$src" -maxdepth 1 -type f | sort)
  elif [ -f "$src" ]; then
    FILES+=("$src")
  else
    die "no such file or directory: $src"
  fi
done
[ "${#FILES[@]}" -gt 0 ] || die "no input files found"

[ "$DRY_RUN" -eq 1 ] || mkdir -p "$DEST"

ok=0; skipped=0; failed=0
for f in "${FILES[@]}"; do
  base="$(basename "$f")"
  if ! is_audio "$base"; then
    note "  skip (not audio) : $base"; skipped=$((skipped+1)); continue
  fi

  name="$(slug "$base")"
  [ -n "$name" ] || name="clip"
  out="$DEST/$name.wav"

  if [ -e "$out" ] && [ "$FORCE" -eq 0 ]; then
    note "  skip (exists)    : $CATEGORY/$name.wav   (use --force to overwrite)"
    skipped=$((skipped+1)); continue
  fi

  if [ "$DRY_RUN" -eq 1 ]; then
    note "  would write      : $CATEGORY/$name.wav   <- $base"
    ok=$((ok+1)); continue
  fi

  if ffmpeg -nostdin -hide_banner -loglevel error -y -i "$f" \
       -af "loudnorm=I=${TARGET_LUFS}:TP=${TARGET_TP}:LRA=${TARGET_LRA}" \
       -ar "$SAMPLE_RATE" -ac "$CHANNELS" -c:a pcm_s16le "$out" 2>/dev/null; then
    note "  imported         : $CATEGORY/$name.wav"
    ok=$((ok+1))
  else
    printf '  FAILED           : %s (ffmpeg could not decode it)\n' "$base" >&2
    rm -f "$out"
    failed=$((failed+1))
  fi
done

echo
if [ "$DRY_RUN" -eq 1 ]; then
  echo "Dry run: $ok would be imported, $skipped skipped."
else
  echo "Imported $ok into $CATEGORY/ ($skipped skipped, $failed failed)."
  echo "Restart SoundBoard to pick up new clips."
fi

# Non-zero only if something genuinely broke, so this composes in a pipeline.
[ "$failed" -eq 0 ]
