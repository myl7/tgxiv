"use client";

import { useRef, useState, useEffect, useCallback, useMemo, Fragment } from "react";
import { ChannelMeta, ExportMessage, ViewMessage, processMessages, formatDateGroup, getAvatarColor } from "@/lib/tdl";
import { MessageBubble } from "./components/message-bubble";
import { Sidebar } from "./components/sidebar";

const BATCH_SIZE = 50; // how many messages to render at once (local)
const FETCH_LIMIT = 100; // how many messages to fetch per API call

type ChannelCache = {
    messages: ViewMessage[];
    hasMore: boolean;
    oldestId: number | null;
    loading: boolean;
};

interface ClientPageProps {
    channels: ChannelMeta[];
}

async function fetchMessages(
    dirName: string,
    before?: number | null,
    limit: number = FETCH_LIMIT,
): Promise<{ channelId: number; messages: ExportMessage[]; hasMore: boolean; oldestId: number | null }> {
    const params = new URLSearchParams();
    if (before != null) params.set("before", String(before));
    params.set("limit", String(limit));
    const url = `/api/channels/${encodeURIComponent(dirName)}/messages?${params.toString()}`;
    const res = await fetch(url);
    if (!res.ok) throw new Error(`Failed to fetch messages: ${res.status}`);
    return res.json();
}

function computeRestoreCount(messages: ViewMessage[], savedMsgId: string): number | null {
    const msgIndex = messages.findIndex((m) => String(m.msgId) === savedMsgId);
    if (msgIndex === -1) return null;
    return Math.min(messages.length - msgIndex + BATCH_SIZE, messages.length);
}

export function ClientPage({ channels }: ClientPageProps) {
    const [selectedChannelId, setSelectedChannelId] = useState(channels[0].channelId);
    const [sidebarOpen, setSidebarOpen] = useState(true);

    // Per-channel caches: Map<channelId, ChannelCache>
    const [channelCaches, setChannelCaches] = useState<Map<number, ChannelCache>>(new Map());

    // How many messages from the tail of the cache to render
    const [loadedCount, setLoadedCount] = useState(0);

    const selectedChannel = useMemo(
        () => channels.find((c) => c.channelId === selectedChannelId) ?? channels[0],
        [channels, selectedChannelId],
    );

    const cache = channelCaches.get(selectedChannelId);
    const allMessages = cache?.messages ?? [];
    const totalCount = allMessages.length;
    const serverHasMore = cache?.hasMore ?? true;
    const isChannelLoading = cache?.loading ?? false;

    const visibleMessages = useMemo(() => {
        const start = Math.max(0, totalCount - loadedCount);
        return allMessages.slice(start);
    }, [allMessages, totalCount, loadedCount]);

    const scrollContainerRef = useRef<HTMLDivElement>(null);
    const sentinelRef = useRef<HTMLDivElement>(null);
    const isLoadingRef = useRef(false);
    const prevScrollHeightRef = useRef(0);
    const pendingRestoreRef = useRef<string | null>(null);
    const loadMoreRef = useRef<() => void>(() => {});
    // Track which channel we're currently restoring scroll for, to avoid stale async work
    const restoreChannelRef = useRef<number | null>(null);

    // Helper: update a single channel's cache entry
    const updateCache = useCallback(
        (channelId: number, updater: (prev: ChannelCache) => ChannelCache) => {
            setChannelCaches((prev) => {
                const existing = prev.get(channelId) ?? {
                    messages: [],
                    hasMore: true,
                    oldestId: null,
                    loading: false,
                };
                const next = new Map(prev);
                next.set(channelId, updater(existing));
                return next;
            });
        },
        [],
    );

    // Fetch the newest chunk for a channel and optionally restore scroll
    const initChannel = useCallback(
        async (channelId: number) => {
            const meta = channels.find((c) => c.channelId === channelId);
            if (!meta) return;

            // Mark loading
            updateCache(channelId, (c) => ({ ...c, loading: true }));

            try {
                const result = await fetchMessages(meta.dirName);
                const processed = processMessages(
                    { id: result.channelId, messages: result.messages },
                    meta.dirName,
                );

                updateCache(channelId, () => ({
                    messages: processed,
                    hasMore: result.hasMore,
                    oldestId: result.oldestId,
                    loading: false,
                }));

                // Set loadedCount for initial display
                const savedMsgId = localStorage.getItem(`tdl-scroll-${channelId}`);
                if (savedMsgId) {
                    const count = computeRestoreCount(processed, savedMsgId);
                    if (count !== null) {
                        setLoadedCount(count);
                        pendingRestoreRef.current = savedMsgId;
                        return;
                    }
                    // savedMsgId not found in this chunk; need to fetch older chunks
                    if (result.hasMore) {
                        restoreChannelRef.current = channelId;
                        await restoreScrollByFetching(
                            meta,
                            channelId,
                            savedMsgId,
                            processed,
                            result.hasMore,
                            result.oldestId,
                        );
                        return;
                    }
                }
                // No saved position or no more data: show newest batch
                setLoadedCount(Math.min(BATCH_SIZE, processed.length));
            } catch (err) {
                console.error("Failed to load channel:", err);
                updateCache(channelId, (c) => ({ ...c, loading: false }));
            }
        },
        // eslint-disable-next-line react-hooks/exhaustive-deps
        [channels, updateCache],
    );

    // Fetch older chunks until we find the saved message ID
    const restoreScrollByFetching = useCallback(
        async (
            meta: ChannelMeta,
            channelId: number,
            savedMsgId: string,
            currentMessages: ViewMessage[],
            currentHasMore: boolean,
            currentOldestId: number | null,
        ) => {
            const olderChunks: ViewMessage[][] = [];
            let hasMore = currentHasMore;
            let oldestId = currentOldestId;

            while (hasMore) {
                if (restoreChannelRef.current !== channelId) return;

                const result = await fetchMessages(meta.dirName, oldestId);
                const processed = processMessages(
                    { id: result.channelId, messages: result.messages },
                    meta.dirName,
                );

                olderChunks.unshift(processed);
                hasMore = result.hasMore;
                oldestId = result.oldestId;

                // Only merge when the target message is in this chunk
                if (processed.some((m) => String(m.msgId) === savedMsgId)) {
                    const accumulated = [...olderChunks.flat(), ...currentMessages];
                    updateCache(channelId, () => ({
                        messages: accumulated,
                        hasMore,
                        oldestId,
                        loading: false,
                    }));

                    const count = computeRestoreCount(accumulated, savedMsgId);
                    if (count !== null) {
                        setLoadedCount(count);
                        pendingRestoreRef.current = savedMsgId;
                    }
                    restoreChannelRef.current = null;
                    return;
                }
            }

            // Exhausted all messages without finding saved ID
            const accumulated = [...olderChunks.flat(), ...currentMessages];
            updateCache(channelId, () => ({
                messages: accumulated,
                hasMore,
                oldestId,
                loading: false,
            }));
            restoreChannelRef.current = null;
            setLoadedCount(Math.min(BATCH_SIZE, accumulated.length));
        },
        [updateCache],
    );

    // On channel selection change: load from cache or fetch
    useEffect(() => {
        const existing = channelCaches.get(selectedChannelId);
        if (existing && existing.messages.length > 0) {
            // Use cached messages, restore scroll
            const savedMsgId = localStorage.getItem(`tdl-scroll-${selectedChannelId}`);
            if (savedMsgId) {
                const count = computeRestoreCount(existing.messages, savedMsgId);
                if (count !== null) {
                    setLoadedCount(count);
                    pendingRestoreRef.current = savedMsgId;
                    return;
                }
            }
            setLoadedCount(Math.min(BATCH_SIZE, existing.messages.length));
        } else {
            // No cache: fetch
            setLoadedCount(0);
            initChannel(selectedChannelId);
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [selectedChannelId]);

    const saveScrollPosition = useCallback(
        (channelId: number) => {
            const container = scrollContainerRef.current;
            if (!container) return;

            const containerRect = container.getBoundingClientRect();
            const elements = container.querySelectorAll<HTMLElement>("[data-msg-id]");

            let lastVisibleMsgId: string | null = null;
            for (const el of elements) {
                const rect = el.getBoundingClientRect();
                if (rect.top < containerRect.bottom) {
                    lastVisibleMsgId = el.getAttribute("data-msg-id");
                } else {
                    break;
                }
            }
            if (lastVisibleMsgId) {
                localStorage.setItem(`tdl-scroll-${channelId}`, lastVisibleMsgId);
            }
        },
        [],
    );

    // Whether there are more local messages to show, or more on the server
    const hasMoreLocal = loadedCount < totalCount;
    const hasMore = hasMoreLocal || serverHasMore;

    const loadMore = useCallback(() => {
        if (isLoadingRef.current || !hasMore) return;

        // No pagination before the first page lands: without a cached page the
        // oldestId cursor is null, which would re-fetch the newest page and duplicate it.
        const existing = channelCaches.get(selectedChannelId);
        if (isChannelLoading || !existing || existing.messages.length === 0) return;

        const container = scrollContainerRef.current;
        if (container) {
            prevScrollHeightRef.current = container.scrollHeight;
        }

        if (hasMoreLocal) {
            // Show more from local cache
            isLoadingRef.current = true;
            setLoadedCount((prev) => Math.min(prev + BATCH_SIZE, totalCount));
        } else if (serverHasMore) {
            // Fetch next chunk from API
            const meta = channels.find((c) => c.channelId === selectedChannelId);
            const currentOldestId = existing.oldestId;
            if (!meta || currentOldestId == null) return;

            isLoadingRef.current = true;

            fetchMessages(meta.dirName, currentOldestId).then((result) => {
                const processed = processMessages(
                    { id: result.channelId, messages: result.messages },
                    meta.dirName,
                );

                const existingIds = new Set(existing.messages.map((m) => m.msgId));
                const fresh = processed.filter((m) => !existingIds.has(m.msgId));

                setChannelCaches((prev) => {
                    const current = prev.get(selectedChannelId);
                    if (!current) return prev;
                    const merged = [...fresh, ...current.messages];
                    const next = new Map(prev);
                    next.set(selectedChannelId, {
                        messages: merged,
                        hasMore: result.hasMore,
                        oldestId: result.oldestId,
                        loading: false,
                    });
                    return next;
                });

                // Increase loadedCount by the number of new messages
                setLoadedCount((prev) => prev + fresh.length);
                isLoadingRef.current = false;
            }).catch((err) => {
                console.error("Failed to load more messages:", err);
                isLoadingRef.current = false;
            });
        }
    }, [hasMore, hasMoreLocal, serverHasMore, isChannelLoading, totalCount, channels, selectedChannelId, channelCaches]);

    useEffect(() => {
        loadMoreRef.current = loadMore;
    }, [loadMore]);

    // Restore scroll position after loading older messages, or scroll to saved position
    useEffect(() => {
        const container = scrollContainerRef.current;

        if (pendingRestoreRef.current) {
            requestAnimationFrame(() => {
                if (!container) return;
                const target = container.querySelector<HTMLElement>(
                    `[data-msg-id="${pendingRestoreRef.current}"]`,
                );
                if (target) {
                    target.scrollIntoView({ block: "start" });
                }
                pendingRestoreRef.current = null;
            });
            return;
        }

        if (container && prevScrollHeightRef.current > 0) {
            const newScrollHeight = container.scrollHeight;
            const diff = newScrollHeight - prevScrollHeightRef.current;
            container.scrollTop += diff;
            prevScrollHeightRef.current = 0;
        }
        isLoadingRef.current = false;
    }, [loadedCount]);

    // Scroll to bottom on initial mount and channel switch (when no saved position)
    useEffect(() => {
        if (pendingRestoreRef.current) return;
        if (isChannelLoading) return;
        requestAnimationFrame(() => {
            const container = scrollContainerRef.current;
            if (container) {
                container.scrollTop = container.scrollHeight;
            }
        });
    }, [selectedChannelId, isChannelLoading]);

    // Track whether user is near the bottom of the scroll container
    const [isAtBottom, setIsAtBottom] = useState(true);
    useEffect(() => {
        const container = scrollContainerRef.current;
        if (!container) return;

        const handleScroll = () => {
            const distanceFromBottom = container.scrollHeight - container.scrollTop - container.clientHeight;
            const nearBottom = distanceFromBottom < 200;
            setIsAtBottom((prev) => (prev === nearBottom ? prev : nearBottom));
        };

        container.addEventListener("scroll", handleScroll, { passive: true });
        return () => container.removeEventListener("scroll", handleScroll);
    }, [selectedChannelId]);

    const scrollToBottom = useCallback(() => {
        const container = scrollContainerRef.current;
        if (container) {
            container.scrollTo({ top: container.scrollHeight, behavior: "smooth" });
        }
    }, []);

    // Save scroll position on page unload
    useEffect(() => {
        const handleBeforeUnload = () => {
            saveScrollPosition(selectedChannelId);
        };
        window.addEventListener("beforeunload", handleBeforeUnload);
        return () => window.removeEventListener("beforeunload", handleBeforeUnload);
    }, [selectedChannelId, saveScrollPosition]);

    // IntersectionObserver for the sentinel at the top
    useEffect(() => {
        const sentinel = sentinelRef.current;
        const container = scrollContainerRef.current;
        if (!sentinel || !container) return;

        const observer = new IntersectionObserver(
            (entries) => {
                if (entries[0].isIntersecting) {
                    loadMoreRef.current();
                }
            },
            {
                root: container,
                rootMargin: "200px 0px 0px 0px",
                threshold: 0,
            },
        );

        observer.observe(sentinel);
        return () => observer.disconnect();
        // Reconnect only when sentinel/container may have changed
    }, [selectedChannelId]);

    // Group messages by date
    const groupedMessages = useMemo(() => {
        const groups: { date: string; messages: ViewMessage[] }[] = [];
        let currentDate = "";

        for (const msg of visibleMessages) {
            const dateStr = formatDateGroup(msg.date);
            if (dateStr !== currentDate) {
                currentDate = dateStr;
                groups.push({ date: dateStr, messages: [msg] });
            } else {
                groups[groups.length - 1].messages.push(msg);
            }
        }

        return groups;
    }, [visibleMessages]);

    const handleSelectChannel = useCallback(
        (newChannelId: number) => {
            saveScrollPosition(selectedChannelId);
            setSelectedChannelId(newChannelId);
        },
        [selectedChannelId, saveScrollPosition],
    );

    return (
        <div className="flex h-dvh bg-[#8ba0b5]">
            {/* Sidebar */}
            <Sidebar
                channels={channels}
                selectedChannelId={selectedChannelId}
                onSelectChannel={handleSelectChannel}
                open={sidebarOpen}
                onClose={() => setSidebarOpen(false)}
            />

            {/* Main content */}
            <div className="flex flex-col flex-1 min-w-0">
                {/* Header */}
                <header className="bg-[#517da2] text-white px-4 py-3 shadow-sm flex items-center gap-3 shrink-0 z-10">
                    {/* Menu button (visible when sidebar is closed) */}
                    {!sidebarOpen && (
                        <button
                            onClick={() => setSidebarOpen(true)}
                            className="text-white/80 hover:text-white cursor-pointer shrink-0"
                        >
                            <svg
                                className="w-6 h-6"
                                fill="none"
                                stroke="currentColor"
                                viewBox="0 0 24 24"
                            >
                                <path
                                    strokeLinecap="round"
                                    strokeLinejoin="round"
                                    strokeWidth={2}
                                    d="M4 6h16M4 12h16M4 18h16"
                                />
                            </svg>
                        </button>
                    )}

                    <div
                        className="w-10 h-10 rounded-full flex items-center justify-center text-white font-bold text-lg shrink-0"
                        style={{
                            backgroundColor: getAvatarColor(selectedChannel.channelId),
                        }}
                    >
                        {selectedChannel.channelName.charAt(0)}
                    </div>
                    <div className="min-w-0">
                        <h1 className="text-lg font-semibold leading-tight truncate">
                            {selectedChannel.channelName}
                        </h1>
                        <p className="text-xs text-blue-100 opacity-80">
                            {selectedChannel.messageCount.toLocaleString()} messages
                        </p>
                    </div>
                </header>

                {/* Message list */}
                <div className="relative flex-1 min-h-0">
                    <div
                        ref={scrollContainerRef}
                        className="h-full overflow-y-auto"
                        style={{
                            backgroundImage: `url("data:image/svg+xml,%3Csvg width='60' height='60' viewBox='0 0 60 60' xmlns='http://www.w3.org/2000/svg'%3E%3Cg fill='none' fill-rule='evenodd'%3E%3Cg fill='%23000000' fill-opacity='0.03'%3E%3Cpath d='M36 34v-4h-2v4h-4v2h4v4h2v-4h4v-2h-4zm0-30V0h-2v4h-4v2h4v4h2V6h4V4h-4zM6 34v-4H4v4H0v2h4v4h2v-4h4v-2H6zM6 4V0H4v4H0v2h4v4h2V6h4V4H6z'/%3E%3C/g%3E%3C/g%3E%3C/svg%3E")`,
                        }}
                    >
                        <div className="max-w-3xl mx-auto px-2 sm:px-4 py-4">
                            {/* Sentinel for infinite scroll (top) */}
                            <div ref={sentinelRef} className="h-1" />

                            {isChannelLoading && totalCount === 0 && (
                                <div className="text-center py-12">
                                    <span className="text-sm text-white/70 bg-black/10 rounded-full px-4 py-2">
                                        Loading messages...
                                    </span>
                                </div>
                            )}

                            {hasMore && !isChannelLoading && (
                                <div className="text-center py-3">
                                    <span className="text-xs text-white/60 bg-black/10 rounded-full px-3 py-1">
                                        Loading older messages...
                                    </span>
                                </div>
                            )}

                            {groupedMessages.map((group, idx) => (
                                <Fragment key={`${group.date}-${idx}`}>
                                    <div className="flex justify-center my-3">
                                        <span className="text-xs text-white bg-black/20 rounded-full px-3 py-1 backdrop-blur-sm">
                                            {group.date}
                                        </span>
                                    </div>

                                    {group.messages.map((msg) => (
                                        <div key={msg.msgId} data-msg-id={msg.msgId}>
                                            <MessageBubble message={msg} />
                                        </div>
                                    ))}
                                </Fragment>
                            ))}
                        </div>
                    </div>

                    {/* Scroll to bottom button */}
                    {!isAtBottom && (
                        <button
                            onClick={scrollToBottom}
                            className="absolute bottom-4 right-4 w-10 h-10 rounded-full bg-white/90 shadow-lg flex items-center justify-center cursor-pointer hover:bg-white transition-colors"
                        >
                            <svg
                                className="w-5 h-5 text-[#517da2]"
                                fill="none"
                                stroke="currentColor"
                                viewBox="0 0 24 24"
                            >
                                <path
                                    strokeLinecap="round"
                                    strokeLinejoin="round"
                                    strokeWidth={2}
                                    d="M19 14l-7 7m0 0l-7-7m7 7V3"
                                />
                            </svg>
                        </button>
                    )}
                </div>
            </div>
        </div>
    );
}
