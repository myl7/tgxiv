import path from "path";
import fs from "fs";
import Database from "better-sqlite3";
import type { ExportMessage } from "@/lib/tdl";

export const CHANNELS_DIR = path.join(process.cwd(), process.env.CHANNELS_DIR || "channels");

export function isWithinDir(base: string, target: string): boolean {
    return path.resolve(target).startsWith(path.resolve(base));
}

// ---- tgca archive layout ----
//
// A channel directory is a tgca archive written by the Go archiver (tgxiv) in
// the repository root: an "archive.db" SQLite database holding every message's
// content (messages), the media download pipeline state (downloads) and
// channel metadata (meta), plus a "media/" dir whose files follow tdl's
// "<channelId>_<msgId>_<name>" template. Legacy "export/" JSON files and
// "logs/" are not read by the viewer.

/** Media directory for a channel. */
export function mediaDir(channelDir: string): string {
    return path.join(channelDir, "media");
}

/** archive.db path for a channel directory. */
export function archiveDbPath(channelDir: string): string {
    return path.join(channelDir, "archive.db");
}

/** Directory names under CHANNELS_DIR that are channel archives. */
export async function listChannelDirs(): Promise<string[]> {
    let entries: fs.Dirent[];
    try {
        entries = await fs.promises.readdir(CHANNELS_DIR, { withFileTypes: true });
    } catch {
        return []; // channels dir missing or unreadable
    }
    const dirs: string[] = [];
    for (const entry of entries) {
        if (!entry.isDirectory()) continue;
        try {
            await fs.promises.access(archiveDbPath(path.join(CHANNELS_DIR, entry.name)));
            dirs.push(entry.name);
        } catch {
            // no archive.db: not a channel archive
        }
    }
    return dirs;
}

// ---- archive.db access ----

// Read-only connections cached per channel directory and reopened when the db
// file is replaced. A readonly WAL reader never takes write locks, so the Go
// archiver can keep writing concurrently.
type DbHandle = { db: Database.Database; mtimeMs: number };
const dbCache = new Map<string, DbHandle>();

/**
 * Open a channel's archive.db read-only, reusing the cached connection while
 * the file is unchanged. Throws (with the channel name) if it is missing or
 * cannot be opened.
 */
function openChannelDb(dirName: string): Database.Database {
    const channelDir = path.join(CHANNELS_DIR, dirName);
    const dbPath = archiveDbPath(channelDir);

    let mtimeMs: number;
    try {
        mtimeMs = fs.statSync(dbPath).mtimeMs;
    } catch (err) {
        throw new Error(`no archive.db for channel "${dirName}": ${(err as Error).message}`);
    }

    const cached = dbCache.get(channelDir);
    if (cached && cached.mtimeMs === mtimeMs) return cached.db;
    if (cached) {
        dbCache.delete(channelDir);
        try {
            cached.db.close();
        } catch {
            // already closed
        }
    }

    try {
        const db = new Database(dbPath, { readonly: true, fileMustExist: true });
        // Brief retry window instead of an immediate SQLITE_BUSY when the
        // archiver checkpoints while we read.
        db.pragma("busy_timeout = 3000");
        dbCache.set(channelDir, { db, mtimeMs });
        return db;
    } catch (err) {
        throw new Error(`failed to open archive.db for channel "${dirName}": ${(err as Error).message}`);
    }
}

type DbMessageRow = {
    id: number;
    type: string;
    date: number;
    text: string;
    file: string;
    raw: string;
};

/** Parse a message's raw JSON blob; null when absent or malformed. */
function parseRaw(raw: string | null): ExportMessage["raw"] {
    if (!raw) return null;
    try {
        return JSON.parse(raw) as NonNullable<ExportMessage["raw"]>;
    } catch {
        return null;
    }
}

/** Cached channel metadata, invalidated by archive.db mtime (cheap stat). */
type CachedMeta = { mtimeMs: number; value: { id: number | null; messageCount: number } };
const metaCache = new Map<string, CachedMeta>();

/**
 * Read channel metadata (id from meta.channel_id, message count) from
 * archive.db. Returns null when the directory is not a channel archive or the
 * database cannot be read.
 */
export async function getChannelMeta(
    dirName: string,
): Promise<{ id: number | null; messageCount: number } | null> {
    const channelDir = path.join(CHANNELS_DIR, dirName);
    const dbPath = archiveDbPath(channelDir);

    let mtimeMs: number;
    try {
        mtimeMs = (await fs.promises.stat(dbPath)).mtimeMs;
    } catch {
        return null; // no archive.db: not a channel archive
    }

    const cached = metaCache.get(channelDir);
    if (cached && cached.mtimeMs === mtimeMs) return cached.value;

    try {
        const db = openChannelDb(dirName);
        const metaRow = db
            .prepare("SELECT value FROM meta WHERE key = 'channel_id'")
            .get() as { value: string } | undefined;
        const parsed = metaRow ? parseInt(metaRow.value, 10) : NaN;
        const messageCount = (
            db.prepare("SELECT COUNT(*) AS n FROM messages").get() as { n: number }
        ).n;
        const value = { id: Number.isFinite(parsed) ? parsed : null, messageCount };
        metaCache.set(channelDir, { mtimeMs, value });
        return value;
    } catch (err) {
        console.error(`tgxiv: reading meta of channel "${dirName}" failed: ${(err as Error).message}`);
        return null;
    }
}

/** One page of messages, mirroring the API response shape. */
export type MessagesPage = {
    channelId: number;
    messages: ExportMessage[]; // ascending by id (oldest first)
    hasMore: boolean;
    oldestId: number | null;
};

// `before` of 0 means "no cursor"; limit+1 rows are fetched to compute hasMore.
const MESSAGES_PAGE_SQL = `
    SELECT msg_id AS id, type, date, file, text, raw
    FROM messages
    WHERE type = 'message' AND (? = 0 OR msg_id < ?)
    ORDER BY msg_id DESC
    LIMIT ?`;

/**
 * Read a page of a channel's messages from archive.db, newest first, starting
 * below the `before` message id (0 = from the newest). The returned page is
 * ascending by id, matching the API response contract.
 */
export async function readMessages(
    dirName: string,
    before: number,
    limit: number,
): Promise<MessagesPage> {
    let rows: DbMessageRow[];
    let channelId: number | null;
    try {
        const db = openChannelDb(dirName);
        rows = db.prepare(MESSAGES_PAGE_SQL).all(before, before, limit + 1) as DbMessageRow[];
        const metaRow = db
            .prepare("SELECT value FROM meta WHERE key = 'channel_id'")
            .get() as { value: string } | undefined;
        const parsed = metaRow ? parseInt(metaRow.value, 10) : NaN;
        channelId = Number.isFinite(parsed) ? parsed : null;
    } catch (err) {
        throw new Error(`failed to read messages of channel "${dirName}": ${(err as Error).message}`);
    }

    const hasMore = rows.length > limit;
    if (hasMore) rows = rows.slice(0, limit);

    const messages: ExportMessage[] = rows
        .map((row) => ({
            id: row.id,
            type: "message" as const,
            date: row.date,
            text: row.text || undefined,
            file: row.file || undefined,
            raw: parseRaw(row.raw),
        }))
        .reverse();

    return {
        channelId: channelId ?? 0,
        messages,
        hasMore,
        oldestId: messages.length > 0 ? messages[0].id : null,
    };
}

/**
 * Locate the downloaded media file of a message via the downloads table. A
 * done row's path is reduced to its basename and resolved inside the
 * channel's media/ dir, so an archive that moved since the download (or a
 * path stored with foreign separators) still resolves, and path traversal is
 * impossible. Returns null when there is no usable row or the file is gone;
 * callers fall back to scanning media/.
 */
export function resolveDownloadPath(dirName: string, msgId: number): string | null {
    let row: { path: string } | undefined;
    try {
        const db = openChannelDb(dirName);
        row = db
            .prepare("SELECT path FROM downloads WHERE msg_id = ? AND status = 'done' AND path <> ''")
            .get(msgId) as { path: string } | undefined;
    } catch {
        return null; // missing/corrupt db: fall back to the media/ scan
    }
    if (!row?.path) return null;

    // normalize separators before taking the basename (Go writes host-native paths)
    const base = path.basename(row.path.replace(/\\/g, "/"));
    if (!base || base === "/" || base === ".") return null;

    const candidate = path.join(mediaDir(path.join(CHANNELS_DIR, dirName)), base);
    try {
        return fs.statSync(candidate).isFile() ? candidate : null;
    } catch {
        return null;
    }
}
