import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { viteSingleFile } from 'vite-plugin-singlefile';

export default defineConfig({
  plugins: [svelte(), viteSingleFile()],
  build: {
    outDir: '../../services/frontend/dist',
    emptyOutDir: false,
    target: 'es2020',
    cssCodeSplit: false,
    assetsInlineLimit: 100000000,
  },
  server: {
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
      '/ws': { target: 'http://localhost:8080', ws: true, changeOrigin: false },
    },
  },
});
