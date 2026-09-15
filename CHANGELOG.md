# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - 2026-09-15
### Added
- One archive root manages any number of dialogs — channels, groups, and
  private chats — in a single `tgxiv.sqlite` (dialogs / messages / tasks
  tables chained by real foreign keys).
- Media stored under `media/<dialog_id>/<msgID>_<file_name>` with locally
  sanitized names: separators, control bytes, and Windows-forbidden
  characters filtered, UUID fallback for unusable ones, no silent truncation.
- Per-file SHA-256 hashes (`tasks.file_hash`) — the basis for future content
  dedup of forwarded media.
- Per-dialog incremental-sync watermarks: `sync` fetches only each dialog's
  new messages.
- `split` moves whole dialogs (rows, media, watermarks) into another archive
  root; an interrupted run is resumable by re-running the same command.
- `download --retry <dialog_id>/<msg_id>` re-runs specific tasks whatever
  their prior status.
- Built-in web viewer (`serve`) reads the archive read-only and streams media
  with Range support; dialogs are listed from the database, not the
  filesystem.
- Idle watchdog (`--idle-timeout`) kills stalled tdl batches so one stuck
  file can no longer freeze an archive run.
- `dialog.txt` pointer files inside each media folder keep numeric dialog
  folders human-navigable.
