import type { NextConfig } from "next";

const apiOrigin = process.env.AUTOLIFE_API_ORIGIN ?? "http://127.0.0.1:8080";

const config: NextConfig = {
  poweredByHeader: false,
  async redirects() {
    return [{ source: "/", destination: "/ru", permanent: false }];
  },
  async rewrites() {
    return [
      { source: "/api/v1/:path*", destination: `${apiOrigin}/api/v1/:path*` },
      { source: "/health/:path*", destination: `${apiOrigin}/health/:path*` },
    ];
  },
};

export default config;
