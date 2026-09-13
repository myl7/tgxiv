import { NextRequest, NextResponse } from "next/server";
import path from "path";
import fs from "fs/promises";
import { createReadStream } from "fs";
import { Readable } from "stream";
import { CHANNELS_DIR, isWithinDir, mediaDir, resolveDownloadPath } from "@/lib/server";

const MIME_TYPES: Record<string, string> = {
    // Images
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
    // Video
    ".mp4": "video/mp4",
    ".webm": "video/webm",
    ".mov": "video/quicktime",
    ".mkv": "video/x-matroska",
    // Audio
    ".mp3": "audio/mpeg",
    ".m4a": "audio/mp4",
    ".aac": "audio/aac",
    ".wav": "audio/wav",
    ".ogg": "audio/ogg",
    ".flac": "audio/flac",
    // Common files
    ".pdf": "application/pdf",
    ".zip": "application/zip",
    ".txt": "text/plain",
};

/**
 * Resolve the media file for "{channelId}_{msgId}" (the last URL segment).
 * First consults archive.db's downloads table (a done row's path, reduced to
 * its basename inside the channel's media/ dir), then falls back to the
 * legacy media/ filename prefix scan. Returns null when nothing is found.
 */
async function resolveMediaFile(
    channelDirName: string,
    fileName: string,
): Promise<string | null> {
    const lastSegment = fileName.slice(fileName.lastIndexOf("/") + 1);
    const ids = lastSegment.match(/^(\d+)_(\d+)/);

    if (ids) {
        const [, channelIdStr, msgIdStr] = ids;
        const msgId = parseInt(msgIdStr, 10);
        const dir = mediaDir(path.join(CHANNELS_DIR, channelDirName));

        // DB lookup first: exact file recorded by the archiver
        const fromDb = resolveDownloadPath(channelDirName, msgId);
        if (fromDb) return fromDb;

        // Fallback: find the file by unique prefix (channelId_msgId_)
        const prefix = `${channelIdStr}_${msgIdStr}_`;
        try {
            const files = await fs.readdir(dir);
            const match = files.find((f) => f.startsWith(prefix));
            if (match) return path.join(dir, match);
        } catch {
            // media dir missing
        }
    }

    return null;
}

export async function GET(
    request: NextRequest,
    { params }: { params: Promise<{ path: string[] }> },
) {
    const { path: segments } = await params;

    // Expect at least 2 segments: [channelDirName, channelId_msgId]
    if (segments.length < 2) {
        return new NextResponse("Bad Request", { status: 400 });
    }

    const channelDirName = decodeURIComponent(segments[0]);
    const fileName = segments.slice(1).join("/");

    const channelDir = path.join(CHANNELS_DIR, channelDirName);
    if (!isWithinDir(CHANNELS_DIR, channelDir)) {
        return new NextResponse("Forbidden", { status: 403 });
    }

    try {
        const matchPath = await resolveMediaFile(channelDirName, fileName);
        if (!matchPath) {
            return new NextResponse("Not Found", { status: 404 });
        }

        const stat = await fs.stat(matchPath);
        if (!stat.isFile()) {
            return new NextResponse("Not Found", { status: 404 });
        }

        const ext = path.extname(matchPath).toLowerCase();
        const contentType = MIME_TYPES[ext] || "application/octet-stream";
        const rangeHeader = request.headers.get("range");

        if (rangeHeader?.startsWith("bytes=")) {
            const [startStr, endStr] = rangeHeader.replace("bytes=", "").split("-");
            const start = Number.parseInt(startStr, 10);
            const end = endStr ? Number.parseInt(endStr, 10) : stat.size - 1;

            if (
                Number.isNaN(start) ||
                Number.isNaN(end) ||
                start < 0 ||
                end < start ||
                end >= stat.size
            ) {
                return new NextResponse("Requested Range Not Satisfiable", {
                    status: 416,
                    headers: {
                        "Content-Range": `bytes */${stat.size}`,
                    },
                });
            }

            const chunkSize = end - start + 1;
            const stream = createReadStream(matchPath, { start, end });

            return new NextResponse(Readable.toWeb(stream) as ReadableStream, {
                status: 206,
                headers: {
                    "Content-Type": contentType,
                    "Content-Length": chunkSize.toString(),
                    "Content-Range": `bytes ${start}-${end}/${stat.size}`,
                    "Accept-Ranges": "bytes",
                    "Cache-Control": "public, max-age=31536000, immutable",
                },
            });
        }

        const stream = createReadStream(matchPath);
        return new NextResponse(Readable.toWeb(stream) as ReadableStream, {
            headers: {
                "Content-Type": contentType,
                "Content-Length": stat.size.toString(),
                "Accept-Ranges": "bytes",
                "Cache-Control": "public, max-age=31536000, immutable",
            },
        });
    } catch {
        return new NextResponse("Not Found", { status: 404 });
    }
}
