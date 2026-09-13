import { NextRequest, NextResponse } from "next/server";
import path from "path";
import type { MessagesResponse } from "@/lib/tdl";
import { CHANNELS_DIR, isWithinDir, readMessages } from "@/lib/server";

const DEFAULT_LIMIT = 100;

export async function GET(
    request: NextRequest,
    { params }: { params: Promise<{ dirName: string }> },
) {
    const { dirName } = await params;
    const channelDirName = decodeURIComponent(dirName);

    const channelPath = path.join(CHANNELS_DIR, channelDirName);
    if (!isWithinDir(CHANNELS_DIR, channelPath)) {
        return NextResponse.json({ error: "Forbidden" }, { status: 403 });
    }

    const url = request.nextUrl;
    const beforeParam = url.searchParams.get("before");
    const limitParam = url.searchParams.get("limit");
    const limit = limitParam ? Math.max(1, Math.min(1000, parseInt(limitParam, 10) || DEFAULT_LIMIT)) : DEFAULT_LIMIT;

    // Cursor: only messages with id < beforeId; 0 (or unparsable) = from the newest
    const beforeId = beforeParam ? parseInt(beforeParam, 10) : NaN;
    const before = Number.isFinite(beforeId) ? beforeId : 0;

    try {
        const page = await readMessages(channelDirName, before, limit);
        const response: MessagesResponse = {
            channelId: page.channelId,
            messages: page.messages,
            hasMore: page.hasMore,
            oldestId: page.oldestId,
        };
        return NextResponse.json(response);
    } catch (err) {
        console.error(`tgxiv: loading messages of channel "${channelDirName}" failed: ${(err as Error).message}`);
        return NextResponse.json({ error: "Not found" }, { status: 404 });
    }
}
