import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
      '/auth': { target: 'http://localhost:8080', changeOrigin: false },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './tests/setup.ts',
    css: true,
    coverage: {
      provider: 'v8',
      reporter: ['text', 'lcov'],
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/main.tsx', 'src/api/types.ts'],
      // Vitest 4 enables AST-based V8 remapping: older thresholds (38/62/38/38)
      // were calibrated against v8-to-istanbul false positives. Keep the
      // measured v4 baseline so CI still rejects regressions.
      thresholds: {
        statements: 33,
        branches: 28,
        functions: 27,
        lines: 35,
      },
    },
  },
})
