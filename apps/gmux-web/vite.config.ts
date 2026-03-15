import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

export default defineConfig({
  plugins: [preact()],
  server: {
    proxy: {
      '/v1': {
        target: 'http://127.0.0.1:8790',
      },
      '/ws': {
        target: 'http://127.0.0.1:8790',
        ws: true,
      },
    },
  },
})
