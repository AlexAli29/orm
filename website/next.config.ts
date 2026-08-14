import type { NextConfig } from "next";

const config: NextConfig = {
  // The site is fully static: every page is a locale and a slug known at build
  // time, and search runs in the browser against an index built here. Nothing
  // needs a server, so nothing gets one.
  output: "export",
  images: { unoptimized: true },
  trailingSlash: true,
};

export default config;
