import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

export default defineConfig({
  base: "/",
  plugins: [react()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  server: {
    port: 5174,
    // Local dev proxies the API to a locally-running service so the SPA
    // can be iterated without a deploy.
    proxy: { "/api": { target: "http://127.0.0.1:8080", changeOrigin: false } },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
});
