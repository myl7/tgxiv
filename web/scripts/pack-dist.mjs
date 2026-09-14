// Pack the Next.js static export (web/out) into the Go embed dir
// (internal/webui/dist) as pre-gzipped files: every file in out/ is written
// to dist/<same-relative-path>.gz via gzipSync at level 9, creating
// directories as needed. Paths resolve relative to this script so the CWD
// does not matter.
//
// Contract: only *.gz files are ever written or pruned under dist/. Stale
// .gz files from previous runs are unlinked first, but every other file is
// left untouched — a git-tracked placeholder index.html lives in dist/ so
// the Go embed pattern matches before the first build, and it must survive.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const outDir = path.join(scriptDir, "..", "out");
const distDir = path.join(scriptDir, "..", "..", "internal", "webui", "dist");

/** All regular files under dir, recursively, as absolute paths. */
function walk(dir) {
    const files = [];
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) files.push(...walk(full));
        else if (entry.isFile()) files.push(full);
    }
    return files;
}

if (!fs.existsSync(outDir)) {
    console.error(`pack-dist: ${outDir} not found; run \`pnpm build\` first`);
    process.exit(1);
}

// Prune stale *.gz output; never touch anything else in dist/.
fs.mkdirSync(distDir, { recursive: true });
for (const file of walk(distDir)) {
    if (file.endsWith(".gz")) fs.unlinkSync(file);
}

let count = 0;
let inBytes = 0;
let outBytes = 0;
for (const file of walk(outDir)) {
    const target = path.join(distDir, path.relative(outDir, file) + ".gz");
    fs.mkdirSync(path.dirname(target), { recursive: true });
    const raw = fs.readFileSync(file);
    const gz = gzipSync(raw, { level: 9 });
    fs.writeFileSync(target, gz);
    count++;
    inBytes += raw.length;
    outBytes += gz.length;
}

console.log(`pack-dist: ${count} files, ${inBytes} bytes -> ${outBytes} bytes (.gz)`);
