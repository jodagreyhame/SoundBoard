# shellcheck shell=bash
#
# clip_dir.sh — resolve the folder SoundBoard actually reads clips from.
#
# Sourced by the import helpers. The clip library used to sit in sounds/ next to
# the repo, so the scripts could assume it; it now lives in the user's Documents
# and can be repointed from the app, so writing to a hard-coded path would drop
# files somewhere SoundBoard never looks.
#
# Resolution order, matching the app:
#   1. $SOUNDBOARD_CLIP_DIR, if set — an explicit override for odd setups.
#   2. clipFolder in the app's config.json, if the user chose one.
#   3. <Documents>/SoundBoard, the default.
#
# Note this reads the CONFIGURED path rather than asking Windows for the
# Documents known folder, so a Documents folder redirected by OneDrive is
# honoured only once the app has stored a path. Fall back accordingly.

soundboard_config_file() {
  if [ -n "${APPDATA:-}" ]; then
    printf '%s\n' "$APPDATA/soundboard/config.json"
    return
  fi
  printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/soundboard/config.json"
}

soundboard_documents_dir() {
  # Prefer a real Documents folder if one is present; USERPROFILE is set under
  # Git Bash / MSYS on Windows.
  local home="${USERPROFILE:-$HOME}"
  printf '%s\n' "$home/Documents"
}

# soundboard_clip_dir prints the clip library path. It does not create it.
soundboard_clip_dir() {
  if [ -n "${SOUNDBOARD_CLIP_DIR:-}" ]; then
    printf '%s\n' "$SOUNDBOARD_CLIP_DIR"
    return
  fi

  local cfg configured
  cfg="$(soundboard_config_file)"
  if [ -f "$cfg" ] && command -v jq >/dev/null 2>&1; then
    configured="$(jq -r '.clipFolder // empty' "$cfg" 2>/dev/null || true)"
    if [ -n "$configured" ]; then
      printf '%s\n' "$configured"
      return
    fi
  fi

  printf '%s\n' "$(soundboard_documents_dir)/SoundBoard"
}
