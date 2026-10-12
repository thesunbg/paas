import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

const api = process.env.PAAS_API ?? 'http://127.0.0.1:8080';

export default defineConfig({
  plugins: [svelte()],
  base: '/',
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    target: 'es2022',
  },
  server: {
    proxy: { '/api': { target: api, changeOrigin: false } },
  },
  preview: {
    proxy: { '/api': { target: api, changeOrigin: false } },
  },
});
