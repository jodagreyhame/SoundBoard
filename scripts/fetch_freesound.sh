#!/usr/bin/env bash
#
# fetch_freesound.sh — fetch Creative-Commons audio from Freesound into the library.
#
# Freesound (https://freesound.org) is a collaborative database of CC-licensed
# audio with a documented public API. This uses that API as intended: your own key,
# no browser impersonation, and every clip's licence and author recorded so you can
# meet the attribution terms.
#
# It fetches Freesound's public MP3 previews, which the API serves with a plain API
# key. Downloading original uploads requires OAuth2, which is deliberately out of
# scope here — previews are 128 kbps MP3 and are fine for a soundboard.
#
# SETUP
#   1. Create a free account at https://freesound.org
#   2. Get an API key at https://freesound.org/apiv2/apply/
#   3. export FREESOUND_API_KEY=your_key_here
#
# USAGE
#   scripts/fetch_freesound.sh <category> <search query> [-n COUNT]
#   scripts/fetch_freesound.sh reactions "applause"
#   scripts/fetch_freesound.sh games "8-bit coin" -n 5
#   scripts/fetch_freesound.sh memes "air horn" --license cc0
#
# OPTIONS
#   -n, --count N        how many clips to fetch (default 5, max 30)
#   -l, --license SPEC   cc0 | by | any   (default: any CC licence)
#                        cc0 needs no attribution; by requires crediting the author
#   -d, --max-duration S skip clips longer than S seconds (default 15)
#   -n, --dry-run        show what would be fetched, download nothing
#   -h, --help           this text
#
# ATTRIBUTION
#   Every fetched clip is appended to sounds/<category>/ATTRIBUTION.md with its
#   Freesound ID, author, licence and URL. CC-BY audio REQUIRES that you credit the
#   author if you redistribute it. Keep that file with the audio.
#
# REQUIRES
#   curl, jq, ffmpeg on PATH.
#
set -uo pipefail

API="https://freesound.org/apiv2/search/text/"
COUNT=5
LICENSE="any"
MAX_DUR=15
DRY_RUN=0

die()  { printf 'fetch_freesound: %s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,/^set -uo/p' "$0" | sed 's/^# \{0,1\}//; $d'; exit "${1:-0}"; }

args=()
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--count)        COUNT="${2:?}"; shift 2;;
    -l|--license)      LICENSE="${2:?}"; shift 2;;
    -d|--max-duration) MAX_DUR="${2:?}"; shift 2;;
    --dry-run)         DRY_RUN=1; shift;;
    -h|--help)         usage 0;;
    -*)                die "unknown option: $1 (try --help)";;
    *)                 args+=("$1"); shift;;
  esac
done

[ "${#args[@]}" -ge 2 ] || usage 1
CATEGORY="${args[0]}"
QUERY="${args[*]:1}"

case "$CATEGORY" in *[/\\]*|.|..|"") die "invalid category: '$CATEGORY'";; esac
[ "$COUNT" -ge 1 ] 2>/dev/null && [ "$COUNT" -le 30 ] || die "--count must be 1-30"

for t in curl jq ffmpeg; do
  command -v "$t" >/dev/null 2>&1 || die "$t not found on PATH"
done
[ -n "${FREESOUND_API_KEY:-}" ] || die "FREESOUND_API_KEY is not set. Get a key at https://freesound.org/apiv2/apply/ then: export FREESOUND_API_KEY=..."

# Freesound filter syntax. Licence names in the API are human-readable strings.
case "$LICENSE" in
  cc0) FILTER='license:"Creative Commons 0"';;
  by)  FILTER='license:"Attribution"';;
  any) FILTER='';;
  *)   die "--license must be cc0, by, or any";;
esac
FILTER="${FILTER:+$FILTER }duration:[0 TO $MAX_DUR]"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/clip_dir.sh
. "$SCRIPT_DIR/clip_dir.sh"
CLIP_DIR="$(soundboard_clip_dir)"
DEST="$CLIP_DIR/$CATEGORY"
ATTR="$DEST/ATTRIBUTION.md"

slug() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' \
    | sed -e 's/[^a-z0-9]\+/_/g' -e 's/_\+/_/g' -e 's/^_//' -e 's/_$//'
}

echo "Searching Freesound for \"$QUERY\" (licence: $LICENSE, max ${MAX_DUR}s)..."

resp="$(curl -sS -G "$API" \
  --data-urlencode "query=$QUERY" \
  --data-urlencode "filter=$FILTER" \
  --data-urlencode "fields=id,name,license,username,duration,previews,url" \
  --data-urlencode "page_size=$COUNT" \
  -H "Authorization: Token $FREESOUND_API_KEY")" || die "search request failed"

if printf '%s' "$resp" | jq -e '.detail' >/dev/null 2>&1; then
  die "API error: $(printf '%s' "$resp" | jq -r '.detail')"
fi

total="$(printf '%s' "$resp" | jq -r '.count // 0')"
got="$(printf '%s' "$resp" | jq -r '.results | length')"
[ "$got" -gt 0 ] || die "no results for \"$QUERY\" with licence=$LICENSE under ${MAX_DUR}s"
echo "$total match; taking $got."
echo

[ "$DRY_RUN" -eq 1 ] || mkdir -p "$DEST"

ok=0; failed=0
while IFS=$'\t' read -r id name license username duration preview page; do
  base="$(slug "$name")"; [ -n "$base" ] || base="freesound_$id"
  out="$DEST/${base}.wav"

  if [ "$DRY_RUN" -eq 1 ]; then
    printf '  would fetch : %-34s %s by %s (%ss)\n' "$CATEGORY/$base.wav" "$license" "$username" "$duration"
    ok=$((ok+1)); continue
  fi

  tmp="$(mktemp -t fs-XXXXXX).mp3"
  if ! curl -sSfL "$preview" -o "$tmp"; then
    printf '  FAILED      : %s (download)\n' "$name" >&2
    rm -f "$tmp"; failed=$((failed+1)); continue
  fi

  # Same normalisation as import_sounds.sh so mixed sources sit at one level.
  if ffmpeg -nostdin -hide_banner -loglevel error -y -i "$tmp" \
       -af "loudnorm=I=-16:TP=-1.5:LRA=11" -ar 48000 -ac 2 -c:a pcm_s16le "$out" 2>/dev/null; then
    printf '  fetched     : %-34s %s by %s\n' "$CATEGORY/$base.wav" "$license" "$username"
    [ -f "$ATTR" ] || {
      printf '# Attribution — %s\n\nAudio fetched from Freesound (https://freesound.org).\nCC-BY clips REQUIRE crediting the author if you redistribute them.\nKeep this file alongside the audio.\n\n| Clip | Freesound ID | Author | Licence | Source |\n|---|---|---|---|---|\n' "$CATEGORY" > "$ATTR"
    }
    printf '| `%s.wav` | %s | %s | %s | %s |\n' "$base" "$id" "$username" "$license" "$page" >> "$ATTR"
    ok=$((ok+1))
  else
    printf '  FAILED      : %s (transcode)\n' "$name" >&2
    rm -f "$out"; failed=$((failed+1))
  fi
  rm -f "$tmp"
done < <(printf '%s' "$resp" | jq -r '.results[] | [.id, .name, .license, .username, (.duration|tostring), .previews["preview-hq-mp3"], .url] | @tsv')

echo
if [ "$DRY_RUN" -eq 1 ]; then
  echo "Dry run: $ok would be fetched."
else
  echo "Fetched $ok into $CATEGORY/ ($failed failed)."
  [ "$ok" -gt 0 ] && echo "Attribution recorded in $CATEGORY/ATTRIBUTION.md — keep it with the audio."
  echo "Restart SoundBoard to pick up new clips."
fi

[ "$failed" -eq 0 ]
