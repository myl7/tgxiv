"use client";

import { useEffect, useState } from "react";
import { DialogMeta } from "@/lib/tdl";
import { ClientPage } from "./client-page";

// Fetch the dialog list from the Go server on mount: newly archived
// dialogs must appear without a rebuild.
export default function Home() {

    // null while loading, [] when the fetch failed or returned nothing
    const [dialogs, setDialogs] = useState<DialogMeta[] | null>(null);

    useEffect(() => {
        let cancelled = false;
        fetch("/api/channels")
            .then((res) => {
                if (!res.ok) throw new Error(`HTTP ${res.status}`);
                return res.json() as Promise<DialogMeta[]>;
            })
            .then((data) => {
                if (!cancelled) setDialogs(Array.isArray(data) ? data : []);
            })
            .catch(() => {
                // server unreachable or bad payload
                if (!cancelled) setDialogs([]);
            });
        return () => {
            cancelled = true;
        };
    }, []);

    if (dialogs === null) {
        return (
            <div className="flex min-h-screen items-center justify-center bg-[#eee]">
                <p className="text-gray-500 text-lg">Loading channels…</p>
            </div>
        );
    }

    if (dialogs.length === 0) {
        return (
            <div className="flex min-h-screen items-center justify-center bg-[#eee]">
                <p className="text-gray-500 text-lg">
                    No channels found. Serve an archive directory with{" "}
                    <code className="bg-gray-200 px-1 rounded">
                        tgxiv serve --dir {"<dir>"}
                    </code>{" "}
                    and archive channels with{" "}
                    <code className="bg-gray-200 px-1 rounded">tgxiv archive</code>.
                </p>
            </div>
        );
    }

    return <ClientPage dialogs={dialogs} />;
}
