"use client";

import { DialogMeta, displayDialogName, getAvatarColor } from "@/lib/tdl";

interface SidebarProps {
    dialogs: DialogMeta[];
    selectedDialogId: number;
    onSelectDialog: (dialogId: number) => void;
    open: boolean;
    onClose: () => void;
}

export function Sidebar({
    dialogs,
    selectedDialogId,
    onSelectDialog,
    open,
    onClose,
}: SidebarProps) {
    // The server orders by dialog id; the sidebar reads better sorted by
    // whatever name the dialog is displayed as.
    const sorted = [...dialogs].sort((a, b) =>
        displayDialogName(a).localeCompare(displayDialogName(b)),
    );

    return (
        <>
            {/* Backdrop overlay for mobile */}
            {open && <div className="fixed inset-0 bg-black/30 z-30 lg:hidden" onClick={onClose} />}

            {/* Sidebar panel */}
            <aside
                className={`fixed top-0 left-0 h-full z-40 bg-white flex flex-col
                    w-[300px] transition-transform duration-200 ease-in-out
                    ${open ? "translate-x-0" : "-translate-x-full"}
                    lg:relative lg:z-auto lg:shrink-0
                    ${open ? "lg:translate-x-0" : "lg:-translate-x-full lg:w-0 lg:overflow-hidden"}`}
            >
                {/* Sidebar header */}
                <div className="bg-[#517da2] text-white px-4 py-3 flex items-center justify-between shrink-0">
                    <h2 className="text-base font-semibold">Channels</h2>
                    <button
                        onClick={onClose}
                        className="text-white/80 hover:text-white text-xl leading-none cursor-pointer"
                    >
                        ✕
                    </button>
                </div>

                {/* Dialog list */}
                <div className="flex-1 overflow-y-auto">
                    {sorted.map((d) => {
                        const name = displayDialogName(d);
                        return (
                            <button
                                key={d.dialogId}
                                onClick={() => onSelectDialog(d.dialogId)}
                                className={`w-full text-left px-3 py-2.5 flex items-center gap-3 cursor-pointer
                                    transition-colors border-b border-gray-100
                                    ${
                                        d.dialogId === selectedDialogId
                                            ? "bg-[#419fd9]/15"
                                            : "hover:bg-gray-50"
                                    }`}
                            >
                                {/* Avatar */}
                                <div
                                    className="w-[50px] h-[50px] rounded-full flex items-center justify-center text-white font-bold text-lg shrink-0"
                                    style={{ backgroundColor: getAvatarColor(d.dialogId) }}
                                >
                                    {name.charAt(0)}
                                </div>

                                {/* Dialog info */}
                                <div className="flex-1 min-w-0">
                                    <div className="flex items-center gap-1.5 min-w-0">
                                        <p className="text-sm font-semibold text-gray-900 truncate">
                                            {name}
                                        </p>
                                        {d.kind && (
                                            <span className="text-[10px] leading-tight text-gray-400 border border-gray-200 rounded px-1 py-px shrink-0">
                                                {d.kind}
                                            </span>
                                        )}
                                    </div>
                                    {d.title && d.username && (
                                        <p className="text-xs text-gray-400 truncate">
                                            @{d.username}
                                        </p>
                                    )}
                                    <p className="text-[11px] text-gray-300">
                                        ID: {d.dialogId} · {d.messageCount.toLocaleString()} msgs
                                    </p>
                                </div>
                            </button>
                        );
                    })}
                </div>
            </aside>
        </>
    );
}
