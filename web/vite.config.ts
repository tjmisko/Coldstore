/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Basic auth wraps the WHOLE mux -- the API, the thumbnails and the SPA -- so
// with credentials configured the browser answers a 401 from the dev proxy with
// its own native dialog and the app's error banner never renders. Injecting the
// header here is what makes `make dev-web` usable against an authenticated
// daemon, and what makes the 401 branch of the banner reachable at all (design
// section 7.10, dev-mode note).
const devUser = process.env.COLDSTORE_USERNAME
const devPassword = process.env.COLDSTORE_PASSWORD
const authHeader =
  devUser && devPassword
    ? { Authorization: 'Basic ' + Buffer.from(`${devUser}:${devPassword}`).toString('base64') }
    : undefined

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      '/api': {
        target: process.env.COLDSTORE_DEV_TARGET ?? 'http://127.0.0.1:8090',
        changeOrigin: false,
        headers: authHeader,
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    css: false,
    restoreMocks: true,
  },
})
