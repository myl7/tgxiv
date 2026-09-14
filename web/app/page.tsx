"use client";

import { useEffect, useState } from "react";
import { ChannelMeta } from "@/lib/tdl";
import { ClientPage } from "./client-page";

// Fetch the channel list from the Go server on mount: newly archived
// channels must appear without a rebuild.
export default function Home() {

    // null while loading, [] when the fetch failed or returned nothing
    const [channels, setChannels] = useState<ChannelMeta[] | null>(null);

    useEffect(() => {
        let cancelled = false;
        fetch("/api/channels")
            .then((res) => {
                if (!res.ok) throw new Error(`HTTP ${res.status}`);
                return res.json() as Promise<ChannelMeta[]>;
            })
            .then((data) => {
                if (!cancelled) setChannels(Array.isArray(data) ? data : []);
            })
            .catch(() => {
                // server unreachable or bad payload
                if (!cancelled) setChannels([]);
            });
        return () => {
            cancelled = true;
        };
    }, []);

    if (channels === null) {
        return (
            <div className="flex min-h-screen items-center justify-center bg-[#eee]">
                <p className="text-gray-500 text-lg">Loading channels…</p>
            </div>
        );
    }

    if (channels.length === 0) {
        return (
            <div className="flex min-h-screen items-center justify-center bg-[#eee]">
                <p className="text-gray-500 text-lg">
                    No channels found. Serve a channels directory with{" "}
                    <code className="bg-gray-200 px-1 rounded">
                        tgxiv serve --channels {"<dir>"}
                    </code>{" "}
                    and archive channels with{" "}
                    <code className="bg-gray-200 px-1 rounded">tgxiv archive</code>.
                </p>
            </div>
        );
    }

    return <ClientPage channels={channels} />;
}
