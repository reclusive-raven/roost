# CLAUDE.md

roost keeps Claude Code and Codex transcripts: hard links in `kept/`, add-only `tar.zst` bundles with manifests in `archive_dir`, a record and a redacted FTS5 index in `data_dir`. README.md is the user contract; keep it true when behaviour changes.

## Commands

```sh
go test ./...
go vet ./...
go run ./cmd/roost --config /path/to/test.toml run --dry-run --list
```

## Rules the code depends on

- Never write to the transcript folders, and only add to `archive_dir`: no delete, replace or create of the folder itself.
- The manifest format (`version: 1`) and bundle naming are read by every existing archive. Change them only in a backward-compatible way.
- Times in the record are integer nanoseconds. A coarser clock makes every unchanged file look changed, and kept links are then never released.
- The only deletion is `removeKept`, gated by `release`, the 24 hour `releaseAfter` and an exact recorded match.
- Tests use synthetic fixtures only; never commit a real transcript.

## Git

Linear history: work on a branch, rebase onto `main`, fast-forward only. Commit titles are lower-case imperative with no prefix.
