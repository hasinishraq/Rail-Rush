import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The API is served by Traefik on :8000 (deploy/docker-compose.yml). Proxying keeps
// the app same-origin, so no CORS is needed and return_url stays on localhost.
const api = process.env.API_URL ?? "http://localhost:8000";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/v1": { target: api, changeOrigin: true },
      "/.well-known": { target: api, changeOrigin: true },
    },
  },
});
