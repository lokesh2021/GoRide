import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The Go API listens on :8080. Everything under /v1 is proxied to it so the SPA
// can call the API same-origin (no backend CORS needed — see web/README.md).
//
// SSE note: the /v1/events streams are text/event-stream. http-proxy (which Vite
// uses under the hood) passes these through fine; we only need to make sure the
// proxy does not buffer. changeOrigin keeps the Host header sane; there is no
// response buffering in http-proxy for streamed bodies, and the backend already
// sets `X-Accel-Buffering: no`. We disable timeouts on the proxy so long-lived
// SSE connections are not cut.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/v1": {
        target: "http://localhost:8080",
        changeOrigin: true,
        // Long-lived SSE: don't time the socket out.
        timeout: 0,
        proxyTimeout: 0,
      },
    },
  },
});
