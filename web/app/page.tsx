import { ChannelMeta, parseChannelDirName } from "@/lib/tdl";
import { getChannelMeta, listChannelDirs } from "@/lib/server";
import { ClientPage } from "./client-page";

// Render the channel list per request: newly archived channels must appear
// without a rebuild.
export const dynamic = "force-dynamic";

export default async function Home() {

    let channels: ChannelMeta[] = [];
    try {
        const dirNames = await listChannelDirs();

        const results = await Promise.all(
            dirNames.map(async (dirName) => {
                const meta = await getChannelMeta(dirName);
                if (!meta) return null;
                const { channelName, channelStrId } = parseChannelDirName(dirName);
                return {
                    dirName,
                    channelName,
                    channelStrId,
                    channelId: meta.id ?? 0,
                    messageCount: meta.messageCount,
                } as ChannelMeta;
            }),
        );

        channels = results.filter((c): c is ChannelMeta => c !== null);
        channels.sort((a, b) => a.channelName.localeCompare(b.channelName));
    } catch {
        // channels dir missing or unreadable
    }

    if (channels.length === 0) {
        return (
            <div className="flex min-h-screen items-center justify-center bg-[#eee]">
                <p className="text-gray-500 text-lg">
                    No channels found. Place channel directories in{" "}
                    <code className="bg-gray-200 px-1 rounded">
                        {process.env.CHANNELS_DIR || "channels"}/
                    </code>
                </p>
            </div>
        );
    }

    return <ClientPage channels={channels} />;
}
