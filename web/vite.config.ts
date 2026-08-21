/// <reference types="vitest/config" />
import { cpSync, existsSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join, resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// pdf.js loads two kinds of asset at runtime that are NOT part of its JS
// bundle: the standard-14 font programs, needed by older PDFs that rely on
// Helvetica and friends without embedding them, and the WebAssembly decoders
// for JPEG 2000 and colour management, needed by scanned documents.
//
// Its defaults fetch both from a CDN. The daemon runs on a LAN with no egress
// -- often behind Tailscale with nothing else reachable at all -- so a CDN
// fetch is not slow, it is a blank page. They have to be served from the
// binary's own embedded filesystem.
//
// cmaps/ is deliberately NOT copied: 1.7 MB for CJK encodings, which this
// installation does not have.
const PDFJS_ASSET_DIRS = ['standard_fonts', 'wasm']

/** Where the app asks for pdf.js's runtime assets. Mirrored in PdfReader.tsx. */
export const PDFJS_ASSET_BASE = '/pdfjs'

function pdfjsAssets(): Plugin {
  const require = createRequire(import.meta.url)
  const resolvePdfjsRoot = () => {
    try {
      return dirname(require.resolve('pdfjs-dist/package.json'))
    } catch {
      return ''
    }
  }

  return {
    name: 'coldstore-pdfjs-assets',

    // In `vite dev` there is no output directory to copy into, so serve the
    // files straight out of node_modules. Without this the reader works in the
    // built bundle and 404s in `make dev-web`, which is the kind of difference
    // that gets found late.
    configureServer(server) {
      const root = resolvePdfjsRoot()
      if (!root) return
      server.middlewares.use((req, res, next) => {
        const url = req.url?.split('?')[0] ?? ''
        if (!url.startsWith(`${PDFJS_ASSET_BASE}/`)) return next()
        const rel = url.slice(PDFJS_ASSET_BASE.length + 1)
        // The request path is joined and then checked against the root, rather
        // than trusted: this middleware serves from node_modules.
        const target = resolve(root, rel)
        if (!target.startsWith(resolve(root) + '/') || !existsSync(target)) return next()
        res.setHeader('Cache-Control', 'no-cache')
        import('node:fs').then(({ createReadStream }) => createReadStream(target).pipe(res))
      })
    },

    // writeBundle rather than generateBundle: these are opaque binaries, not
    // things to run through the asset pipeline, and copying them verbatim
    // keeps their names stable so pdf.js's own path building still works.
    writeBundle(options) {
      const root = resolvePdfjsRoot()
      const outDir = options.dir
      if (!root || !outDir) {
        throw new Error('cannot locate pdfjs-dist; the PDF reader would 404 at runtime')
      }
      for (const dir of PDFJS_ASSET_DIRS) {
        const from = join(root, dir)
        if (!existsSync(from)) {
          throw new Error(`pdfjs-dist has no ${dir}/; the PDF reader would fail on some documents`)
        }
        cpSync(from, join(outDir, PDFJS_ASSET_BASE.slice(1), dir), { recursive: true })
      }
    },
  }
}

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
  plugins: [react(), tailwindcss(), pdfjsAssets()],
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
