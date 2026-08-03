import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// This is an explicit, release-only asset build. Go and CMake never invoke it:
// a fresh checkout embeds placeholder.html until scripts/build-webui.sh runs.
export default defineConfig({
  base: '/',
  plugins: [vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    assetsDir: 'assets',
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.js'],
    restoreMocks: true,
    clearMocks: true,
  },
})
