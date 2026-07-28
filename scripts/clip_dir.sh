# shellcheck shell=bash
#
# clip_dir.sh — resolve the folder SoundBoard actually reads clips from.
#
# Sourced by the import helpers. The clip library used to sit in sounds/ next to
# the repo, so the scripts could assume it; it now lives in the user's Documents
# and can be repointed from the app.
#
# This deliberately does NOT guess. `$HOME/Documents` is the wrong answer on any
# machine where OneDrive folder backup or Folder Redirection has moved the
# Documents known folder — which is common — and depositing a batch of clips
# into a directory the app never reads is precisely the silent failure this
# whole change exists to remove. Better to refuse and say why.
#
# Resolution order:
#   1. $SOUNDBOARD_CLIP_DIR — explicit override, wins outright.
#   2. clipFolder in config.json — set when the user picks a folder in the app.
#   3. clipfolder.path beside config.json — the resolved path SoundBoard records
#      on every successful start, which is what covers the default install.
# If none of those yield a directory, the caller must abort.

soundboard_config_dir() {
  if [ -n "${APPDATA:-}" ]; then
    printf '%s\n' "$APPDATA/soundboard"
    return
  fi
  printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/soundboard"
}

# soundboard_clip_dir prints the clip library path, or nothing if it cannot be
# determined. It does not create the directory.
soundboard_clip_dir() {
  if [ -n "${SOUNDBOARD_CLIP_DIR:-}" ]; then
    printf '%s\n' "$SOUNDBOARD_CLIP_DIR"
    return 0
  fi

  local cfgdir cfg breadcrumb configured
  cfgdir="$(soundboard_config_dir)"
  cfg="$cfgdir/config.json"
  breadcrumb="$cfgdir/clipfolder.path"

  if [ -f "$cfg" ]; then
    if command -v jq >/dev/null 2>&1; then
      configured="$(jq -r '.clipFolder // empty' "$cfg" 2>/dev/null || true)"
      if [ -n "$configured" ]; then
        printf '%s\n' "$configured"
        return 0
      fi
    else
      # Do not quietly fall through: a chosen folder may be sitting in a config
      # this script cannot read.
      echo "clip_dir: jq is not installed, so '$cfg' cannot be read." >&2
      echo "clip_dir: install jq, or set SOUNDBOARD_CLIP_DIR to your clip folder." >&2
      return 1
    fi
  fi

  if [ -f "$breadcrumb" ]; then
    # Trailing newline and any stray CR from a Windows-written file.
    configured="$(tr -d '\r\n' < "$breadcrumb")"
    if [ -n "$configured" ]; then
      printf '%s\n' "$configured"
      return 0
    fi
  fi

  echo "clip_dir: cannot determine SoundBoard's clip folder." >&2
  echo "clip_dir: run SoundBoard once so it records the folder, or set" >&2
  echo "clip_dir: SOUNDBOARD_CLIP_DIR=/path/to/your/clips" >&2
  return 1
}

# soundboard_require_clip_dir prints the clip folder or exits non-zero. Callers
# should use this rather than soundboard_clip_dir so a failure cannot silently
# become an empty path.
soundboard_require_clip_dir() {
  local dir
  dir="$(soundboard_clip_dir)" || exit 1
  if [ -z "$dir" ]; then
    echo "clip_dir: resolved an empty clip folder path." >&2
    exit 1
  fi
  if [ ! -d "$dir" ]; then
    echo "clip_dir: clip folder '$dir' does not exist." >&2
    echo "clip_dir: run SoundBoard once to create it, or set SOUNDBOARD_CLIP_DIR." >&2
    exit 1
  fi
  printf '%s\n' "$dir"
}
