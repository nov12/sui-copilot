import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  // Share the icons embedded by Go with the development server and production build.
  publicDir: '../internal/httpapi/static/icons',
  server: {
    port: 5173,
    proxy: { '/api': 'http://127.0.0.1:8080' },
  },
})
