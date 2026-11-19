#!/bin/bash
# Check that the installed launcher can reach the archive folder, and open
# the Full Disk Access pane if it cannot. Run after install.sh installs a new
# launcher. Full Disk Access has no prompt: add
#   ~/Library/Application Support/roost/roost-launcher
# with the + button (Cmd-Shift-G types a path), then run this again.
set -euo pipefail
cd "$(dirname "$0")"

dir="$HOME/Library/Application Support/roost"
[[ -x "$dir/roost-launcher" && -x build/disclaim ]] || { echo "run install.sh first" >&2; exit 1; }
archive=$("$dir/roost" config archive_dir)

if build/disclaim "$dir/roost-launcher" /bin/ls "$archive" >/dev/null 2>&1; then
  echo "ok: the launcher can read $archive"
else
  echo "denied: add $dir/roost-launcher to Full Disk Access, then run this again."
  open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"
  exit 1
fi
