"use client";

import { useState } from "react";
import Zoom from "react-medium-image-zoom";
import "react-medium-image-zoom/dist/styles.css";
import { ViewMessage, renderTextParts, Entity } from "@/lib/tdl";
import { LazyMedia } from "./lazy-media";

interface MessageBubbleProps {
    message: ViewMessage;
}

function RawJsonModal({
    raw,
    msgId,
    onClose,
}: {
    raw: Record<string, unknown>;
    msgId: number;
    onClose: () => void;
}) {
    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm"
            onClick={onClose}
        >
            <div
                className="bg-white rounded-xl shadow-2xl max-w-2xl w-[90vw] max-h-[80vh] flex flex-col"
                onClick={(e) => e.stopPropagation()}
            >
                <div className="flex items-center justify-between px-4 py-3 border-b border-gray-200">
                    <h2 className="text-sm font-semibold text-gray-700">
                        Raw JSON — Message #{msgId}
                    </h2>
                    <button
                        onClick={onClose}
                        className="text-gray-400 hover:text-gray-600 text-lg leading-none px-1"
                    >
                        ✕
                    </button>
                </div>
                <pre className="flex-1 overflow-auto p-4 text-xs text-gray-800 font-mono whitespace-pre-wrap break-all">
                    {JSON.stringify(raw, null, 2)}
                </pre>
            </div>
        </div>
    );
}

function TextContent({ text, entities }: { text: string; entities?: Entity[] }) {
    const parts = renderTextParts(text, entities);

    return (
        <div className="whitespace-pre-wrap wrap-break-word text-sm leading-relaxed text-black">
            {parts.map((part, i) =>
                part.type === "text" ? (
                    <span key={i}>{part.content}</span>
                ) : (
                    <a
                        key={i}
                        href={part.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="text-[#3390ec] hover:underline"
                    >
                        {part.text}
                    </a>
                ),
            )}
        </div>
    );
}

function FileCard({ message }: { message: ViewMessage }) {
    const [expanded, setExpanded] = useState(false);
    const previewable = message.previewKind !== "none";

    if (!message.fileUrl) return null;

    const toggleLabel = expanded
        ? "Hide preview"
        : "Preview as " + message.previewKind;

    const squareClasses = "w-10 h-10 rounded-lg bg-[#3390ec] flex items-center justify-center shrink-0";

    const glyph = (() => {
        switch (message.previewKind) {
            case "image":
                return (
                    <path
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        strokeWidth={2}
                        d="M2.25 15.75l5.159-5.159a2.25 2.25 0 013.182 0l5.159 5.159m-1.5-1.5l1.409-1.409a2.25 2.25 0 013.182 0l2.909 2.909M3.75 21h16.5A1.5 1.5 0 0021.75 19.5V4.5A1.5 1.5 0 0020.25 3H3.75A1.5 1.5 0 002.25 4.5v15A1.5 1.5 0 003.75 21z"
                    />
                );
            case "video":
                return (
                    <>
                        <path
                            strokeLinecap="round"
                            strokeLinejoin="round"
                            strokeWidth={2}
                            d="M21 12a9 9 0 11-18 0 9 9 0 0118 0z"
                        />
                        <path
                            strokeLinecap="round"
                            strokeLinejoin="round"
                            strokeWidth={2}
                            d="M15.91 11.672a.375.375 0 010 .656l-5.603 3.113a.375.375 0 01-.557-.328V8.887c0-.286.307-.466.557-.327l5.603 3.112z"
                        />
                    </>
                );
            case "audio":
                return (
                    <path
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        strokeWidth={2}
                        d="M9 9l10.5-3m0 6.553v3.75a2.25 2.25 0 01-1.632 2.163l-1.32.377a1.803 1.803 0 11-.99-3.467l2.31-.66a2.25 2.25 0 001.632-2.163zm0 0V2.25L9 5.25v10.303m0 0v3.75a2.25 2.25 0 01-1.632 2.163l-1.32.377a1.803 1.803 0 01-.99-3.467l2.31-.66A2.25 2.25 0 009 15.553z"
                    />
                );
            default:
                return (
                    <path
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        strokeWidth={2}
                        d="M12 10v6m0 0l-3-3m3 3l3-3m2 8H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z"
                    />
                );
        }
    })();

    const icon = (
        <svg
            className="w-5 h-5 text-white"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
        >
            {glyph}
        </svg>
    );

    return (
        <div className="mt-2 flex flex-col bg-[#f0f4f8] hover:bg-[#e4eaf0] transition-colors rounded-lg p-3">
            <div className="flex items-center gap-3">
                {previewable ? (
                    <button
                        type="button"
                        onClick={() => setExpanded((v) => !v)}
                        aria-label={toggleLabel}
                        title={toggleLabel}
                        className={`${squareClasses} hover:bg-[#2b7cd3] cursor-pointer`}
                    >
                        {icon}
                    </button>
                ) : (
                    <div className={squareClasses}>{icon}</div>
                )}
                <a
                    href={message.fileUrl}
                    download={message.originalFileName}
                    className="flex-1 min-w-0"
                >
                    <p className="text-sm font-medium text-[#3390ec] hover:underline wrap-break-word">
                        {message.originalFileName}
                    </p>
                    <p className="text-xs text-gray-400">Download file</p>
                </a>
            </div>
            {expanded && previewable && (
                <div className="mt-2">
                    {message.previewKind === "image" && (
                        <LazyMedia src={message.fileUrl} once className="block mt-1 min-h-24 rounded-lg bg-gray-100">
                            {(src) => src ? (
                                <Zoom>
                                    {/* eslint-disable-next-line @next/next/no-img-element */}
                                    <img
                                        src={src}
                                        alt={message.originalFileName || "image"}
                                        className="max-w-full max-h-100 rounded-lg object-contain cursor-zoom-in"
                                        loading="lazy"
                                    />
                                </Zoom>
                            ) : null}
                        </LazyMedia>
                    )}
                    {message.previewKind === "video" && (
                        <LazyMedia src={message.fileUrl} once={false} className="mt-1 min-h-24 rounded-lg bg-gray-100">
                            {(src) => src ? (
                                <video
                                    src={src}
                                    controls
                                    preload="metadata"
                                    className="max-w-full max-h-100 rounded-lg"
                                >
                                    Your browser does not support video playback.
                                </video>
                            ) : null}
                        </LazyMedia>
                    )}
                    {message.previewKind === "audio" && (
                        <LazyMedia src={message.fileUrl} once={false} className="mt-1 w-full min-h-10 rounded-lg bg-gray-100">
                            {(src) => src ? (
                                <audio
                                    src={src}
                                    controls
                                    preload="metadata"
                                    className="w-full h-8"
                                />
                            ) : null}
                        </LazyMedia>
                    )}
                </div>
            )}
        </div>
    );
}

function MediaContent({ message }: { message: ViewMessage }) {
    if (!message.fileUrl || message.mediaKind === "none") return null;

    switch (message.mediaKind) {
        case "image":
            return (
                <LazyMedia src={message.fileUrl} once className="block mt-1 min-h-24 rounded-lg bg-gray-100">
                    {(src) => src ? (
                        <Zoom>
                            {/* eslint-disable-next-line @next/next/no-img-element */}
                            <img
                                src={src}
                                alt={message.originalFileName || "image"}
                                className="max-w-full max-h-100 rounded-lg object-contain cursor-zoom-in"
                                loading="lazy"
                            />
                        </Zoom>
                    ) : null}
                </LazyMedia>
            );
        case "video":
            return (
                <LazyMedia src={message.fileUrl} once={false} className="mt-1 min-h-24 rounded-lg bg-gray-100">
                    {(src) => src ? (
                        <video
                            src={src}
                            controls
                            preload="metadata"
                            className="max-w-full max-h-100 rounded-lg"
                        >
                            Your browser does not support video playback.
                        </video>
                    ) : null}
                </LazyMedia>
            );
        case "audio":
            return (
                <LazyMedia src={message.fileUrl} once={false} className="mt-1 w-full min-h-10 rounded-lg bg-gray-100">
                    {(src) => src ? (
                        <div>
                            {message.originalFileName && (
                                <p className="text-xs text-gray-500 wrap-break-word mb-1">
                                    {message.originalFileName}
                                </p>
                            )}
                            <audio
                                src={src}
                                controls
                                preload="metadata"
                                className="w-full h-8"
                            />
                        </div>
                    ) : null}
                </LazyMedia>
            );
        case "file":
            return <FileCard message={message} />;
        default:
            return null;
    }
}

export function MessageBubble({ message }: MessageBubbleProps) {
    const [showRaw, setShowRaw] = useState(false);
    const hasText = !!message.text;
    const hasMedia = message.mediaKind !== "none" && !!message.fileUrl;
    const isMediaOnly = hasMedia && !hasText;
    const isImageOnly = isMediaOnly && message.mediaKind === "image";

    const footerPillClass = isImageOnly
        ? "text-white bg-black/40 rounded px-1.5 py-0.5"
        : "text-gray-400";

    return (
        <div className="flex mb-1 px-1">
            <div
                className={`relative max-w-[85%] sm:max-w-[65%] rounded-xl shadow-sm ${
                    isMediaOnly && message.mediaKind === "audio" ? "w-full" : ""
                } ${
                    isImageOnly
                        ? "bg-transparent overflow-hidden"
                        : "bg-white px-3 py-1.5"
                }`}
            >
                {hasText && <TextContent text={message.text!} entities={message.entities} />}
                {hasMedia && <MediaContent message={message} />}

                <div
                    className={`flex items-center justify-end gap-2 mt-0.5 ${
                        isImageOnly ? "absolute bottom-2 right-2" : ""
                    }`}
                >
                    <span className={`text-[11px] ${footerPillClass}`}>
                        {message.timeText}
                    </span>
                    <span className={`text-[11px] ${footerPillClass}`}>
                        #{message.msgId}
                    </span>
                    {message.raw && (
                        <button
                            onClick={() => setShowRaw(true)}
                            className={`text-[11px] cursor-pointer hover:underline ${
                                isImageOnly
                                    ? "text-white bg-black/40 rounded px-1.5 py-0.5"
                                    : "text-[#3390ec]"
                            }`}
                        >
                            Raw
                        </button>
                    )}
                </div>
            </div>

            {/* Raw JSON Modal */}
            {showRaw && message.raw && (
                <RawJsonModal
                    raw={message.raw}
                    msgId={message.msgId}
                    onClose={() => setShowRaw(false)}
                />
            )}
        </div>
    );
}
