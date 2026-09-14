// ---- Data Types ----

export type ExportMessage = {
    id: number;
    type: "message";
    file?: string;
    date: number;
    text?: string;
    // Parsed from archive.db's messages.raw JSON blob; null when absent or
    // malformed (the server pre-parses it before it reaches the client).
    raw?: {
        Entities?: Array<{
            Offset?: number;
            Length?: number;
            URL?: string;
        }> | null;
        Media?: {
            Photo?: unknown;
            Document?: { MimeType?: string; Attributes?: Array<Record<string, unknown> | null> } | null;
        } | null;
    } | null;
};

export type ExportData = {
    id: number;
    messages: ExportMessage[];
};

/** API response for paginated messages. */
export type MessagesResponse = {
    channelId: number;
    messages: ExportMessage[];
    hasMore: boolean;
    oldestId: number | null;
};

/** Metadata about a channel parsed from its directory and JSON. */
export type ChannelMeta = {
    dirName: string;
    channelName: string;
    channelStrId?: string;
    channelId: number;
    messageCount: number;
};

/** Parse "{channel_name}[_@{channel_str_id}]" directory name. */
export function parseChannelDirName(dirName: string): {
    channelName: string;
    channelStrId?: string;
} {
    const match = dirName.match(/^(.+?)_@(.+)$/);
    if (match) {
        return { channelName: match[1], channelStrId: match[2] };
    }
    return { channelName: dirName };
}

export type MediaKind = "image" | "video" | "audio" | "file" | "none";

export type Entity = {
    offset: number;
    length: number;
    url: string;
};

export type ViewMessage = {
    channelId: number;
    msgId: number;
    date: number;
    timeText: string;
    text?: string;
    entities?: Entity[];
    originalFileName?: string;
    fileUrl?: string;
    mediaKind: MediaKind;
    /** Content kind a file card can preview as ("none" = not previewable). */
    previewKind: MediaKind;
    raw?: Record<string, unknown>;
};

// ---- Constants ----

const ATTACHMENTS_BASE_PATH = "downloads";

const IMAGE_EXTS = new Set(["jpg", "jpeg", "png", "gif", "webp", "bmp"]);
const VIDEO_EXTS = new Set(["mp4", "webm", "mov", "mkv"]);
const AUDIO_EXTS = new Set(["mp3", "m4a", "aac", "wav", "ogg", "flac"]);

const AVATAR_COLORS = [
    "#7bc862", "#e17076", "#faa774", "#6ec9cb",
    "#65aadd", "#ee7aae", "#a695e7", "#e8a64e",
];

export function getAvatarColor(id: number): string {
    return AVATAR_COLORS[id % AVATAR_COLORS.length];
}

// ---- Helpers ----

export function getMediaKind(fileName: string): MediaKind {
    if (!fileName) return "none";
    const ext = fileName.split(".").pop()?.toLowerCase() ?? "";
    if (IMAGE_EXTS.has(ext)) return "image";
    if (VIDEO_EXTS.has(ext)) return "video";
    if (AUDIO_EXTS.has(ext)) return "audio";
    return "file";
}

/**
 * Classify media by the message's raw send-type, not the file extension:
 * inline players only for documents carrying the video/audio attribute;
 * sent-as-file videos/audios and image documents stay file cards.
 * Extension sniffing (getMediaKind) remains only as the no-raw fallback.
 */
function mediaKindFromRaw(raw: ExportMessage["raw"], fileName: string): MediaKind {
    if (raw?.Media?.Photo) return "image";
    if (raw?.Media?.Document) {
        const attrs = raw.Media.Document.Attributes ?? [];
        const isVideo = attrs.some((a) => !!a && "SupportsStreaming" in a);
        const isAudio = attrs.some((a) => !!a && "Voice" in a);
        if (isVideo) return "video";
        if (isAudio) return "audio";
        return "file";
    }
    return getMediaKind(fileName);
}

/**
 * Content kind of a document from its MimeType; only meaningful for
 * file cards (player kinds get a matching previewKind harmlessly).
 */
function previewKindFromDocument(raw: ExportMessage["raw"]): MediaKind {
    const mime = raw?.Media?.Document?.MimeType?.toLowerCase() ?? "";
    if (mime.startsWith("image/")) return "image";
    if (mime.startsWith("video/")) return "video";
    if (mime.startsWith("audio/")) return "audio";
    return "none";
}

function formatDate(d: Date): string {
    const month = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${d.getFullYear()}-${month}-${day}`;
}

export function formatTime(unixSeconds: number): string {
    const d = new Date(unixSeconds * 1000);
    const hours = String(d.getHours()).padStart(2, "0");
    const minutes = String(d.getMinutes()).padStart(2, "0");
    return `${formatDate(d)} ${hours}:${minutes}`;
}

export function formatDateGroup(unixSeconds: number): string {
    return formatDate(new Date(unixSeconds * 1000));
}

/**
 * Pre-process raw export messages into ViewMessage objects.
 * Messages are sorted by id ascending (oldest first).
 */
export function processMessages(data: ExportData, channelDirName: string): ViewMessage[] {
    const channelId = data.id;

    const sorted = [...data.messages]
        .filter((m) => m.type === "message")
        .sort((a, b) => a.id - b.id);

    return sorted.map((msg) => {
        const hasFile = !!msg.file;
        const mediaKind = hasFile ? mediaKindFromRaw(msg.raw, msg.file!) : "none";
        // Photos always preview as images; documents preview by their real
        // content type even when rendered as a file card.
        const previewKind = !hasFile
            ? "none"
            : mediaKind === "image"
                ? "image"
                : previewKindFromDocument(msg.raw);
        const fileUrl = hasFile
            ? `/${ATTACHMENTS_BASE_PATH}/${encodeURIComponent(channelDirName)}/${channelId}_${msg.id}`
            : undefined;

        // Extract valid entities
        const entities = extractValidEntities(msg);

        return {
            channelId,
            msgId: msg.id,
            date: msg.date,
            timeText: formatTime(msg.date),
            text: msg.text || undefined,
            entities: entities.length > 0 ? entities : undefined,
            originalFileName: hasFile ? msg.file : undefined,
            fileUrl,
            mediaKind,
            previewKind,
            raw: msg.raw ?? undefined,
        };
    });
}

// ---- Entity extraction ----

/**
 * Extract valid entities from a message, sorted by offset, with overlaps removed.
 */
function extractValidEntities(msg: ExportMessage): Entity[] {
    const text = msg.text;
    const rawEntities = msg.raw?.Entities;
    if (!text || !rawEntities || !Array.isArray(rawEntities)) return [];

    // Filter to only valid entities
    const valid: Entity[] = [];
    for (const e of rawEntities) {
        if (
            typeof e.Offset === "number" &&
            e.Offset >= 0 &&
            typeof e.Length === "number" &&
            e.Length > 0 &&
            typeof e.URL === "string" &&
            e.URL.length > 0 &&
            e.Offset + e.Length <= text.length
        ) {
            valid.push({ offset: e.Offset, length: e.Length, url: e.URL });
        }
    }

    // Sort by offset ascending
    valid.sort((a, b) => a.offset - b.offset);

    // Remove overlapping entities (keep earlier ones)
    const result: Entity[] = [];
    let lastEnd = 0;
    for (const ent of valid) {
        if (ent.offset < lastEnd) continue; // overlaps with previous
        result.push(ent);
        lastEnd = ent.offset + ent.length;
    }

    return result;
}

// ---- URL linkify ----

const URL_REGEX =
    /https?:\/\/(?:[^\s<>[\](){}'"，。！？；：、]+)(?<![.,;:!?)}\]'"。，！？；：、])/g;

export type TextPart =
    | { type: "text"; content: string }
    | { type: "link"; text: string; url: string };

function linkifyString(text: string): TextPart[] {
    const parts: TextPart[] = [];
    let lastIndex = 0;

    for (const match of text.matchAll(URL_REGEX)) {
        const start = match.index!;
        if (start > lastIndex) {
            parts.push({ type: "text", content: text.slice(lastIndex, start) });
        }
        parts.push({ type: "link", text: match[0], url: match[0] });
        lastIndex = start + match[0].length;
    }

    if (lastIndex < text.length) {
        parts.push({ type: "text", content: text.slice(lastIndex) });
    }

    return parts;
}

/**
 * Render text with entity links and linkified URLs.
 * - If entities are provided, render entity regions as links, then linkify the rest.
 * - If no entities, linkify the entire text.
 */
export function renderTextParts(text: string, entities?: Entity[]): TextPart[] {
    if (!entities || entities.length === 0) {
        return linkifyString(text);
    }

    const parts: TextPart[] = [];
    let cursor = 0;

    for (const ent of entities) {
        // Linkify text before this entity
        if (ent.offset > cursor) {
            parts.push(...linkifyString(text.slice(cursor, ent.offset)));
        }
        // Render entity as a link
        parts.push({
            type: "link",
            text: text.slice(ent.offset, ent.offset + ent.length),
            url: ent.url,
        });
        cursor = ent.offset + ent.length;
    }

    // Linkify remaining text after last entity
    if (cursor < text.length) {
        parts.push(...linkifyString(text.slice(cursor)));
    }

    return parts;
}
