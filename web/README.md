# tgxiv web (viewer)

Formerly the standalone tdl-viewer; now the web UI of the tgxiv monorepo (see ../README.md).

Telegram Web-style viewer for browsing multiple tgxiv channel archives (`archive.db` + media) with playback

## Features

- Multi-channel support with a collapsible sidebar for switching between channels
- Telegram Web-style chat bubble layout with infinite scroll
- Rich text rendering with hyperlink entities and automatic URL detection
- Inline image preview with click-to-zoom
- Video and audio playback with seek support
- File download for non-media attachments
- Per-message Raw JSON inspector

### Supported media

| Type  | Extensions                      |
| ----- | ------------------------------- |
| Image | jpg, jpeg, png, gif, webp, bmp  |
| Video | mp4, webm, mov, mkv             |
| Audio | mp3, m4a, aac, wav, ogg, flac   |
| File  | everything else (download only) |

## Get Started

### 1. Archive a channel

Create an archive with the Go archiver (tgxiv) in the repository root, which stores messages and download state in a per-channel SQLite database and downloads media into a single directory:

```
my_channel/
  archive.db         SQLite database (messages, downloads, meta) read by the viewer
  media/             downloaded attachments
  export/            legacy tdl JSON exports (not read by the viewer)
  logs/              archiver logs (ignored by the viewer)
```

### 2. Organize channel data

Place each archive directory under `channels/`:

```
channels/
  my_channel/
    archive.db
    media/
  another_channel_@someid/
    archive.db
    media/
```

Directory names follow the format `{channel_name}` or `{channel_name}_@{channel_str_id}`. The `_@{channel_str_id}` suffix is optional and will be displayed in the sidebar when present.

A directory counts as a channel only when it contains an `archive.db`. Messages are read live from the database (schema v2, written by the archiver), so incremental archiver runs are picked up without re-merging files. Legacy `export/` JSON files are no longer read.

### 3. Run the viewer

```bash
pnpm install
pnpm dev
```

Open http://localhost:3000.

## Config

| Option         | Type            | Default    | Description                                             |
| -------------- | --------------- | ---------- | ------------------------------------------------------- |
| `CHANNELS_DIR` | Environment var | `channels` | Path to the directory containing channel subdirectories |

Attachments are resolved by looking up the message's done row in `archive.db` (`downloads` table) and serving the file by its basename from the channel's `media/` directory, so renamed files (e.g. spaces replaced with underscores by `tdl dl`) and archives that moved since the download still resolve. When no done row or file exists, a fallback scan matches media files by the `{channel_id}_{msg_id}_` prefix.

## Limitations

- Designed for local offline browsing only; no authentication or access control
- No message editing, replying, searching, or real-time sync

## License

Copyright (C) 2026 Yulong Ming <i@myl.moe>.

Apache License, Version 2.0.
