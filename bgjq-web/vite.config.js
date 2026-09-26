import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      // 代理 API 请求到 Go 服务端
      '/api': {
        target: 'http://localhost:8888',
        changeOrigin: true,
      },
      // 代理 WebSocket 请求
      '/ws': {
        target: 'ws://localhost:8888',
        ws: true,
        changeOrigin: true,
      }
    }
  },
  build: {
    outDir: 'dist',
    // 生产环境构建相对路径
    base: '/',
  }
})
