import type { NextConfig } from "next";

const CONTROL_PLANE_URL = process.env.CONTROL_PLANE_URL || 'http://control-plane:8080';

const nextConfig: NextConfig = {
  async rewrites() {
    return [
      // Generic API proxy. SSE-streaming endpoints (chat) live under /stream/* so
      // they're served by file-based route handlers and never hit this rewrite,
      // which buffers responses in dev mode.
      { source: '/api/:path*', destination: `${CONTROL_PLANE_URL}/api/:path*` },
    ];
  },
};

export default nextConfig;
