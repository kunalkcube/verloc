import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// SPA is embedded by internal/server via //go:embed all:static
export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: '/dash/',
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  server: {
    port: 8789,
    proxy: {
      '/dash/api': 'http://localhost:18080',
    },
  },
  build: {
    outDir: '../internal/server/static',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // Split the heavy, rarely-first-paint dependencies out of the
        // entry chunk: the login screen used to download all of Recharts.
        manualChunks: (id) => {
          if (id.includes('node_modules/recharts')) return 'charts';
          if (id.includes('node_modules/lucide-react')) return 'icons';
          return undefined;
        },
      },
    },
  },
})
