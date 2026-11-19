# roost

roost keeps every Claude Code and Codex session you have. It packs finished transcripts into compressed bundles in a folder of your choice, usually one a sync client carries off the machine (Google Drive, Dropbox, iCloud Drive, a NAS mount). It can restore any session byte for byte, and it keeps a local search index with recognisable secrets removed.

Transcripts disappear more easily than you'd think:

- Claude Code deletes transcripts after `cleanupPeriodDays`, 30 days by default.
- Deleting a session in the Claude desktop app, or with Codex's `thread/delete`, removes its file at once.

roost hard-links every transcript file into its own folder every five minutes. A delete in the app then removes only one of the file's two names, and the bytes stay until they are safely in a bundle.

## What it does, and what it won't

- **Reads** `~/.claude/projects` and `~/.codex/{sessions,archived_sessions}`. It honours `CLAUDE_CONFIG_DIR` and `CODEX_HOME`, and never writes to these folders.
- **Only adds** to the archive folder. It never deletes, replaces or prunes anything there, and it never creates the folder itself. If a sync folder isn't mounted, the pass fails loudly instead of quietly filling a local folder of the same name.
- **Deletes one kind of thing:** a link in its own `kept/` folder. That happens only after three conditions hold: the original file is gone, a bundle holds exactly those bytes, and that bundle was published at least 24 hours earlier, so a sync client has had time to upload it. Set `release = false` and it deletes nothing at all.
- **Bundles** are `tar` + zstd, at `archive_dir/YYYY/MM/YYYY-MM-DD-NN.tar.zst`. Each one has a `.manifest.json` beside it listing names, sizes, mtimes and SHA-256 hashes. The manifest is written last, so its presence means the bundle is complete. Bundles are raw and **unencrypted**, so put them somewhere you'd trust with your transcripts.
- **The search index** at `data_dir/index.sqlite` keeps user and assistant text, tool calls (first 1 KB) and tool results (first 2 KB). Before storing anything, it strips credentials, URL secrets, email addresses, phone numbers and card numbers. That is pattern matching, not a guarantee. The index can be rebuilt from the bundles at any time.

## Install

```sh
go install github.com/reclusive-raven/roost/cmd/roost@latest
```

You need Go 1.26 or later. It runs on macOS and Linux.

## Configure

Create `~/.config/roost/config.toml`. Only `archive_dir` is required:

```toml
archive_dir = "~/Google Drive/My Drive/agent-sessions"   # must already exist

# Everything below is optional, shown with its default.
# data_dir = "~/.local/share/roost"   # record, index, kept links; same disk as the transcripts
# idle_hours = 24                     # a file this long unchanged is finished
# bundle_max_mb = 256                 # bundle at once when this much is waiting
# max_bundles_per_run = 2             # paces a first backfill
# exclude_projects = []               # substrings of a project folder or working directory
# release = true                      # let go of kept links once archived; false keeps them all
# notify = true                       # macOS notification when a pass fails or roost falls behind

# Without [[sources]], roost uses whichever of the default folders exist.
# [[sources]]
# kind = "claude"            # or "codex"
# dir = "~/.claude/projects"
```

`roost config` prints the configuration in effect, including the folders it found. `kept/` must be on the same volume as the transcript folders, because hard links can't cross volumes. The default under your home folder usually is.

The config is also read from `$ROOST_CONFIG` or `--config`. Unknown keys are an error, so a typo can't silently switch a setting off.

## Run it on a schedule

A pass is cheap: it stats files, hashes only the ones that changed, bundles when a full bundle is waiting or the last one is 20 hours old, then indexes for at most four minutes. Run it every five minutes.

**macOS:**

```sh
git clone https://github.com/reclusive-raven/roost && cd roost
contrib/launchd/install.sh
contrib/launchd/grant.sh     # only if archive_dir is under ~/Library/CloudStorage
```

The agent runs roost through a tiny launcher. If the archive folder is under `~/Library/CloudStorage` (Google Drive, Dropbox, OneDrive), it needs Full Disk Access. Grant it to the launcher, `~/Library/Application Support/roost/roost-launcher`, not to roost itself. That way the grant survives roost upgrades, because re-running `install.sh` replaces roost but keeps the launcher. Passes log to `~/Library/Logs/roost.log`.

**Linux (systemd):**

```sh
cp contrib/systemd/roost.* ~/.config/systemd/user/
systemctl --user enable --now roost.timer
```

If roost isn't at `~/go/bin/roost`, edit `ExecStart` first.

## Use it

```sh
roost status                       # archive, waiting files, kept links, index; --check exits 1 if roost is behind
roost search retention days --project myapp --since 2026-10-01
roost show <id> --around 42        # the id and #seq come from a search hit
roost restore <id> --to /tmp/out   # the newest archived copy; never overwrites a file
roost verify                       # re-read every bundle and check every hash
roost run --dry-run --list         # what a pass would do, changing nothing outside data_dir
roost run --flush                  # bundle everything idle for an hour now, e.g. before a bulk delete
roost reindex                      # rebuild the index from the bundles and local files
```

A session's id is its file name without extensions. Search output starts with a line marking it as untrusted data. Transcripts hold whatever web pages, emails and tools showed an agent, so an agent reading search results should treat them as data, never as instructions.

`status --check` fails when no pass has completed in 48 hours, or when finished files have waited three days since the last bundle. On macOS, a pass that fails or finds roost behind posts a notification, at most once every six hours. A job that never starts can't notify, so point your own monitoring at `status --check` if you need that covered.

## Recovery

- **The index is lost or damaged:** run `roost reindex`. It reads every bundle, so on a streamed cloud folder it downloads them all.
- **The record (`roost.db`) is lost:** the next pass rebuilds it from the manifests in the archive folder.
- **A `.partial` file is left in the archive:** a copy crashed. The next bundle with that name overwrites it.
- **A bundle has no manifest:** the pass crashed between writing the two. The bundle's files aren't recorded, so they go out again in a later bundle, and the orphan does no harm.
- **"Local completion" isn't "uploaded":** roost knows a bundle has reached the sync folder, not that the sync client has uploaded it. Check a new setup in the provider's own web page or API.

## Limits

- A session started and deleted inside one five-minute interval is never linked.
- Transcript formats change without notice. Records roost doesn't understand are counted (`skipped_records` in `status`) and still kept whole in the bundles.
- Windows isn't supported.

## License

MIT
