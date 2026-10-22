import { resolve } from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The build lands next to the Go package that embeds it (internal/httpapi,
// see static.go), so `make ui && make build` is all a release needs.
//
// In development run the wallet on a fixed port
// (`WALLETSPACE_ADDR=127.0.0.1:8080 make start`) and then `pnpm dev`: Vite
// proxies /api to it. Open the dev server with the token the wallet printed,
// e.g. http://localhost:5173/#token=<token>. Set WALLETSPACE_DEV_API if the
// wallet listens somewhere other than 127.0.0.1:8080.
const api = process.env.WALLETSPACE_DEV_API ?? "http://127.0.0.1:8080"

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": resolve(import.meta.dirname, "./src"),
    },
  },
  build: {
    outDir: resolve(import.meta.dirname, "../internal/httpapi/ui/dist"),
    emptyOutDir: true,
    // The wallet's CSP allows fonts and images from 'self' only; an asset
    // inlined as a data: URI would be refused.
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    proxy: {
      "/api": {
        target: api,
        changeOrigin: true,
        // The guard compares Origin with the host it listens on; the dev
        // server's own origin would be refused as cross-origin.
        configure: (proxy) => {
          proxy.on("proxyReq", (req) => {
            req.removeHeader("origin")
            req.removeHeader("sec-fetch-site")
          })
        },
      },
    },
  },
})
