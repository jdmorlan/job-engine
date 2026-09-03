import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The web client is a client of the same API the CLI uses (D15/D23). In dev the
// Vite server proxies to a control plane; in the container the assets are served
// beside it and /v1 is same-origin, so nothing here leaks into the app code.
export default defineConfig({
  plugins: [react()],
  // Built into the Go package that embeds it, so `make build` produces a binary
  // that already carries the client (D23).
  build: { outDir: '../internal/webui/dist', emptyOutDir: true },
  server: {
    port: 5273,
    proxy: {
      '/v1': {
        target: process.env.JE_ADDR || 'http://127.0.0.1:7620',
        changeOrigin: true,
      },
    },
  },
})
