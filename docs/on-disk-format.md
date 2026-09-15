# On-disk format (1.x contract)

This document specifies what a tgxiv archive root looks like on disk, as
frozen with the 1.0.0 release. Other tools may build against this layout, and
tgxiv will not change it within the 1.x line without a documented, additive
transition. For what tgxiv does and how to drive it, see the
[README](../README.md).

## Layout

```
<root>/
  tgxiv.sqlite            # SQLite: dialogs + messages content + tasks state
  media/<dialog_id>/      # downloaded files, named <msgID>_<file_name>
  export/<dialog_id>/     # transient batch.json + export JSON during runs
  logs/                   # failed-<dialog_id>-<timestamp>.txt reports
```

## Conventions

The contract pins the following: dialog ids are bare positive Telegram ids;
usernames are stored without the `@`; `tasks.path` is always root-relative
with `/` separators; `dialogs.last_msg_id` is each dialog's private
incremental-sync watermark. Every `media/<dialog_id>/` also holds a small
`dialog.txt` key-value pointer file tgxiv maintains — `dialog_id` always,
`title`/`username` lines when known (username in its stored no-`@` form) — so
a person browsing the tree can tell which channel a numeric folder belongs to;
the name `dialog.txt` is reserved and can never collide with a download
(files are always `<msgID>_-prefixed`).

One root hosts any number of dialogs — channels, groups, and private chats
alike — one row each in `dialogs`, one subdirectory each under `media/`. The
`--dir, -d` flag (env `TGXIV_DIR`) points at this root for every command.

## File names

File names are sanitized locally before the download: path separators,
control bytes, and Windows-forbidden characters are replaced with `_`,
trailing dots and spaces are trimmed, and a media name that is empty or has
nothing usable left becomes a UUID. `tasks` records the resulting on-disk
name in `file_name_disk` whenever it differs from the media-provided one —
sane names are stored and written verbatim. There is deliberately no length
truncation: on Windows the effective path limit for tgxiv is 260 UTF-16
characters (Go binaries embed no long-path-aware manifest), so a root prefix
that is too deep surfaces as ordinary download failures whose error text
names the path length — an honest failure beats a silently renamed file.

## Content hashes

Every completed download is content-hashed: the task row stores the SHA-256
of the file's bytes (lowercase hex) in `file_hash` — the basis for a future
content-dedup feature, since forwarded media is common in channels. Rows that
predate 1.0.0 keep an empty hash until their file is re-downloaded.

## WAL

The database runs in WAL mode: `tgxiv.sqlite-wal` and `tgxiv.sqlite-shm` sit
next to it while any tgxiv process has the root open. When cold-copying a
root, stop tgxiv first (or copy all three files together), or the most recent
writes may be left behind in the WAL.
