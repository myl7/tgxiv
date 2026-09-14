import type { NextConfig } from "next";

// Static export served by the Go binary; only dev gets a proxy config because
// `next build` with output: "export" rejects a rewrites key.
const nextConfig: NextConfig = {
    // Disable image optimization since we serve local files
    images: {
        unoptimized: true,
    },
    output: "export",
    // Dev-only proxy to the Go server so `pnpm dev` works without Node routes.
    ...(process.env.NODE_ENV === "development"
        ? {
              async rewrites() {
                  const origin = process.env.TGXIV_API_ORIGIN || "http://127.0.0.1:8080";
                  return [
                      { source: "/api/:path*", destination: `${origin}/api/:path*` },
                      { source: "/downloads/:path*", destination: `${origin}/downloads/:path*` },
                  ];
              },
          }
        : {}),
};

export default nextConfig;
