/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// `npm run dev` serves the app on :5173 and forwards /api to a running Plinth
// (for example `make demo`), so the browser sees one origin.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true, sourcemap: false, target: 'es2022' },
  server: { port: 5173, proxy: { '/api': 'http://127.0.0.1:8080' } },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test-setup.ts'],
    css: false,
  },
})
