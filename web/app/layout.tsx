import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
    title: "Telegram Downloader Viewer",
    description: "Telegram channel message viewer",
};

export default function RootLayout({
    children,
}: Readonly<{
    children: React.ReactNode;
}>) {
    return (
        <html lang="en">
            <body className="antialiased">{children}</body>
        </html>
    );
}
