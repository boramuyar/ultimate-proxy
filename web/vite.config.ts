import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// In development the dashboard talks to a proxy on localhost:8080 (or
// PROXY_URL); in the container nginx forwards /admin the same way.
const target = process.env.PROXY_URL ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(__dirname, "./src") } },
  build: { chunkSizeWarningLimit: 900 },
  server: { proxy: { "/admin": target, "/healthz": target } },
  preview: { proxy: { "/admin": target, "/healthz": target } },
});
