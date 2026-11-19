#!/bin/bash
# Install roost as a launchd agent that runs a pass every five minutes.
#
#   contrib/launchd/install.sh [--replace] [--no-load]
#
# The agent runs roost through a small launcher in
# ~/Library/Application Support/roost, which is the process macOS holds
# responsible for the job's file access. Give the launcher Full Disk Access
# once with grant.sh. TCC keys that grant on the launcher's code hash, so this
# script refuses to replace a launcher that differs from this build unless
# given --replace (then run grant.sh again). roost itself is replaced freely.
#
# --no-load writes everything but leaves the agent unloaded, so the grant
# can be checked before the first pass. ROOST_LABEL overrides the job label.
set -euo pipefail
cd "$(dirname "$0")"

replace=0 load=1
for arg in "$@"; do
  case "$arg" in
    --replace) replace=1 ;;
    --no-load) load=0 ;;
    *) echo "usage: $0 [--replace] [--no-load]" >&2; exit 2 ;;
  esac
done

label=${ROOST_LABEL:-io.github.reclusive-raven.roost}
dest_dir="$HOME/Library/Application Support/roost"
launcher="$dest_dir/roost-launcher"
bin="$dest_dir/roost"
plist="$HOME/Library/LaunchAgents/$label.plist"
log="$HOME/Library/Logs/roost.log"

cdhash() { codesign -dvvv "$1" 2>&1 | awk -F= '/^CDHash=/ {print $2}'; }

# The launcher's cdhash depends on how -o is spelled, so it is linked from
# inside build/ with a bare name: an unchanged tree rebuilds the same binary.
mkdir -p build
(cd build && clang -O2 -Wall -o roost-launcher ../launcher.c &&
  codesign --force --sign - --identifier roost-launcher roost-launcher 2>/dev/null &&
  clang -O2 -Wall -o disclaim ../disclaim.c)
(cd ../.. && go build -trimpath -ldflags "-X github.com/reclusive-raven/roost/internal/roost.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o contrib/launchd/build/roost ./cmd/roost)

build/roost config archive_dir >/dev/null || { echo "set up the config first (see README)" >&2; exit 1; }

new=$(cdhash build/roost-launcher)
mkdir -p "$dest_dir"
if [[ ! -e "$launcher" ]] || (( replace )); then
  cp build/roost-launcher "$launcher.new" && mv -f "$launcher.new" "$launcher"
  echo "launcher: installed $new. Run contrib/launchd/grant.sh now."
elif [[ "$(cdhash "$launcher")" == "$new" ]]; then
  echo "launcher: installed copy matches this build, left in place"
else
  echo "launcher: installed cdhash $(cdhash "$launcher") differs from this build's $new." >&2
  echo "Replacing it drops its Full Disk Access. Re-run with --replace, then run grant.sh." >&2
  exit 1
fi
# A new inode, so a pass already running keeps its own binary.
cp build/roost "$bin.new" && mv -f "$bin.new" "$bin"
echo "roost: installed $("$bin" version)"

mkdir -p "$(dirname "$plist")" "$(dirname "$log")"
cat > "$plist.new" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array><string>$launcher</string><string>$bin</string><string>run</string></array>
  <key>StartInterval</key><integer>300</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>$log</string>
</dict>
</plist>
PLIST
mv -f "$plist.new" "$plist"
echo "agent: wrote $plist"

(( load )) || { echo "agent: not loaded (--no-load). Load with: launchctl bootstrap gui/$(id -u) \"$plist\""; exit 0; }
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
# A bootstrap straight after a bootout can fail while launchd still holds the
# old job, so give it a few tries.
for _ in 1 2 3 4 5; do
  if launchctl bootstrap "gui/$(id -u)" "$plist" 2>/dev/null; then
    echo "agent: loaded; passes log to $log"
    exit 0
  fi
  sleep 2
done
echo "agent: bootstrap failed. Run: launchctl bootstrap gui/$(id -u) \"$plist\"" >&2
exit 1
