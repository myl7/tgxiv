import type { NextConfig } from "next";

const nextConfig: NextConfig = {
    // Disable image optimization since we serve local files
    images: {
        unoptimized: true,
    },
};

export default nextConfig;
