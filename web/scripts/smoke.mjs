// Smoke test for the archive.db data path (mirrors the queries in lib/server.ts
// and app/downloads/[...path]/route.ts against a hand-made fixture archive).
//
// Run: node scripts/smoke.mjs

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import Database from "better-sqlite3";

// ---- fixture: a channels dir with two channel archives + a stray dir ----

const CHANNELS_DIR = fs.mkdtempSync(path.join(os.tmpdir(), "tgxiv-smoke-"));
const DEMO_DIR = path.join(CHANNELS_DIR, "demo_@100");
const ALPHA_DIR = path.join(CHANNELS_DIR, "alpha_@200");
const STRAY_DIR = path.join(CHANNELS_DIR, "legacy_@300");

// Schema mirroring internal/store/store.go (schema v2).
const DDL = `
CREATE TABLE IF NOT EXISTS messages (
    msg_id INTEGER PRIMARY KEY,
    type   TEXT    NOT NULL DEFAULT 'message',
    date   INTEGER NOT NULL DEFAULT 0,
    text   TEXT    NOT NULL DEFAULT '',
    file   TEXT    NOT NULL DEFAULT '',
    raw    TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS downloads (
    msg_id      INTEGER PRIMARY KEY,
    dialog_id   INTEGER NOT NULL,
    file_name   TEXT    NOT NULL DEFAULT '',
    size        INTEGER NOT NULL DEFAULT 0,
    media_type  TEXT    NOT NULL DEFAULT '',
    date        INTEGER NOT NULL DEFAULT 0,
    status      TEXT    NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    actual_size INTEGER NOT NULL DEFAULT 0,
    path        TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '2');
`;

function makeChannel(dir, channelId) {
    fs.mkdirSync(path.join(dir, "media"), { recursive: true });
    const db = new Database(path.join(dir, "archive.db"));
    db.exec(DDL);
    if (channelId !== undefined) {
        db.prepare("INSERT OR REPLACE INTO meta (key, value) VALUES ('channel_id', ?)").run(
            String(channelId),
        );
    }
    return db;
}

const demo = makeChannel(DEMO_DIR, 100);

const insertMessage = demo.prepare(
    "INSERT INTO messages (msg_id, type, date, text, file, raw) VALUES (?, ?, ?, ?, ?, ?)",
);
insertMessage.run(1, "message", 1700000100, "hello world", "", '{"Entities":null}');
insertMessage.run(2, "service", 1700000200, "", "", ""); // filtered out by type
insertMessage.run(
    3,
    "message",
    1700000300,
    "link text",
    "photo.jpg",
    '{"Entities":[{"Offset":0,"Length":4,"URL":"https://example.com/"}]}',
);
insertMessage.run(5, "message", 1700000500, "text only", "", "");
insertMessage.run(7, "message", 1700000700, "broken raw", "", "not json");

const insertDownload = demo.prepare(
    `INSERT INTO downloads
     (msg_id, dialog_id, file_name, size, media_type, date, status, attempts, actual_size, path, error, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
);
// done row whose stored path points at a stale pre-move location
insertDownload.run(
    3, 100, "photo.jpg", 8, "photo", 1700000300, "done", 1, 8,
    "/moved/archive/location/media/100_3_photo.jpg", "", 1700000301,
);
// pending row: must be ignored
insertDownload.run(9, 100, "video.mp4", 9, "video", 1700000900, "pending", 0, 0, "", "", 0);
// done row with empty path: must be ignored
insertDownload.run(11, 100, "doc.pdf", 1, "file", 1700001100, "done", 0, 0, "", "", 0);
// done row attempting traversal: basename keeps resolution inside media/
insertDownload.run(
    13, 100, "secret", 1, "file", 1700001300, "done", 1, 1,
    "../../etc/passwd", "", 1700001301,
);
demo.close();

// dummy media file named per tdl's "<channelId>_<msgId>_<name>" template
fs.writeFileSync(path.join(DEMO_DIR, "media", "100_3_photo.jpg"), "photobytes");

const alpha = makeChannel(ALPHA_DIR); // no channel_id meta
alpha
    .prepare("INSERT INTO messages (msg_id, type, date, text, file, raw) VALUES (?, ?, ?, ?, ?, ?)")
    .run(10, "message", 1700001000, "a10", "", "");
alpha
    .prepare("INSERT INTO messages (msg_id, type, date, text, file, raw) VALUES (?, ?, ?, ?, ?, ?)")
    .run(20, "message", 1700002000, "a20", "", "");
alpha.close();

// legacy export dir without archive.db: not a channel
fs.mkdirSync(path.join(STRAY_DIR, "export"), { recursive: true });
fs.writeFileSync(path.join(STRAY_DIR, "export", "20240101-000000.json"), "{}");

// ---- helpers mirroring lib/server.ts ----

function openChannelDb(dirName) {
    return new Database(path.join(CHANNELS_DIR, dirName, "archive.db"), {
        readonly: true,
        fileMustExist: true,
    });
}

// Mirrors listChannelDirs()
function listChannelDirs() {
    return fs
        .readdirSync(CHANNELS_DIR, { withFileTypes: true })
        .filter((e) => e.isDirectory())
        .filter((e) => {
            try {
                fs.accessSync(path.join(CHANNELS_DIR, e.name, "archive.db"));
                return true;
            } catch {
                return false;
            }
        })
        .map((e) => e.name)
        .sort();
}

// Mirrors getChannelMeta()
function channelMeta(dirName) {
    const db = openChannelDb(dirName);
    const row = db.prepare("SELECT value FROM meta WHERE key = 'channel_id'").get();
    const count = db.prepare("SELECT COUNT(*) AS n FROM messages").get().n;
    db.close();
    const parsed = row ? parseInt(row.value, 10) : NaN;
    return { id: Number.isFinite(parsed) ? parsed : null, messageCount: count };
}

// Mirrors readMessages(): the exact pagination SQL plus page shaping
const PAGE_SQL = `
    SELECT msg_id AS id, type, date, file, text, raw
    FROM messages
    WHERE type = 'message' AND (? = 0 OR msg_id < ?)
    ORDER BY msg_id DESC
    LIMIT ?`;

function readPage(dirName, before, limit) {
    const db = openChannelDb(dirName);
    const rows = db.prepare(PAGE_SQL).all(before, before, limit + 1);
    const metaRow = db.prepare("SELECT value FROM meta WHERE key = 'channel_id'").get();
    db.close();

    const hasMore = rows.length > limit;
    const page = (hasMore ? rows.slice(0, limit) : rows).map((r) => ({
        id: r.id,
        type: r.type,
        date: r.date,
        text: r.text,
        file: r.file,
        raw: r.raw ? tryParse(r.raw) : null,
    }));
    page.reverse(); // ascending by id
    const parsed = metaRow ? parseInt(metaRow.value, 10) : NaN;
    return {
        channelId: Number.isFinite(parsed) ? parsed : 0,
        messages: page,
        hasMore,
        oldestId: page.length > 0 ? page[0].id : null,
    };
}

function tryParse(raw) {
    try {
        return JSON.parse(raw);
    } catch {
        return null;
    }
}

// Mirrors resolveDownloadPath()
function resolveDownloadPath(dirName, msgId) {
    const db = openChannelDb(dirName);
    const row = db
        .prepare("SELECT path FROM downloads WHERE msg_id = ? AND status = 'done' AND path <> ''")
        .get(msgId);
    db.close();
    if (!row?.path) return null;
    const base = path.basename(row.path.replace(/\\/g, "/"));
    const candidate = path.join(CHANNELS_DIR, dirName, "media", base);
    try {
        return fs.statSync(candidate).isFile() ? candidate : null;
    } catch {
        return null;
    }
}

// ---- assertions ----

const checks = [];
function check(name, fn) {
    try {
        fn();
        checks.push(`PASS ${name}`);
    } catch (err) {
        checks.push(`FAIL ${name}: ${err.message}`);
        process.exitCode = 1;
    }
}

check("discovery: only dirs with archive.db are channels", () => {
    assert.deepEqual(listChannelDirs(), ["alpha_@200", "demo_@100"]);
});

check("discovery: dir name parse {name}_@{strId}", () => {
    const m = "demo_@100".match(/^(.+?)_@(.+)$/);
    assert.equal(m[1], "demo");
    assert.equal(m[2], "100");
});

check("meta: channel_id and message count (COUNT(*) incl. non-message rows)", () => {
    assert.deepEqual(channelMeta("demo_@100"), { id: 100, messageCount: 5 });
    assert.deepEqual(channelMeta("alpha_@200"), { id: null, messageCount: 2 });
});

check("pagination: first page of 2, hasMore, ascending, oldestId", () => {
    const page = readPage("demo_@100", 0, 2);
    assert.equal(page.channelId, 100);
    assert.equal(page.hasMore, true);
    assert.deepEqual(page.messages.map((m) => m.id), [5, 7]);
    assert.equal(page.oldestId, 5);
});

check("pagination: before cursor excludes newer ids", () => {
    const page = readPage("demo_@100", 5, 2);
    assert.equal(page.hasMore, false);
    assert.deepEqual(page.messages.map((m) => m.id), [1, 3]);
    assert.equal(page.oldestId, 1);
});

check("pagination: cursor below oldest yields empty page, hasMore false", () => {
    const page = readPage("demo_@100", 1, 10);
    assert.deepEqual(page.messages, []);
    assert.equal(page.hasMore, false);
    assert.equal(page.oldestId, null);
});

check("pagination: limit equal to remaining rows -> hasMore false", () => {
    const page = readPage("demo_@100", 0, 4);
    assert.equal(page.hasMore, false);
    assert.deepEqual(page.messages.map((m) => m.id), [1, 3, 5, 7]);
});

check("pagination: service rows are filtered, raw parsed/guarded per row", () => {
    const page = readPage("demo_@100", 0, 100);
    assert.ok(!page.messages.some((m) => m.id === 2));
    const withEntities = page.messages.find((m) => m.id === 3);
    assert.deepEqual(withEntities.raw.Entities, [
        { Offset: 0, Length: 4, URL: "https://example.com/" },
    ]);
    assert.equal(withEntities.file, "photo.jpg");
    const broken = page.messages.find((m) => m.id === 7);
    assert.equal(broken.raw, null); // malformed raw -> null
});

check("resolution: done row resolves via basename despite stale stored path", () => {
    const resolved = resolveDownloadPath("demo_@100", 3);
    assert.equal(resolved, path.join(DEMO_DIR, "media", "100_3_photo.jpg"));
});

check("resolution: pending row / empty path / missing file -> null", () => {
    assert.equal(resolveDownloadPath("demo_@100", 9), null);
    assert.equal(resolveDownloadPath("demo_@100", 11), null);
    assert.equal(resolveDownloadPath("demo_@100", 12345), null);
});

check("resolution: traversal path stays inside media/", () => {
    const resolved = resolveDownloadPath("demo_@100", 13);
    assert.equal(resolved, null); // basename "passwd" not in media/
    const mediaDir = path.join(DEMO_DIR, "media");
    assert.ok(
        path.join(mediaDir, "passwd").startsWith(path.resolve(mediaDir)),
        "basename join cannot escape media/",
    );
});

console.log(`fixture: ${CHANNELS_DIR}`);
for (const line of checks) console.log(line);
const failed = checks.filter((c) => c.startsWith("FAIL")).length;
console.log(failed === 0 ? "smoke: all checks passed" : `smoke: ${failed} check(s) failed`);

fs.rmSync(CHANNELS_DIR, { recursive: true, force: true });
process.exit(process.exitCode ?? 0);
