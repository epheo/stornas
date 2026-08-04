import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			compilerOptions: {
				runes: ({ filename }) =>
					filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			},
			// Standalone SPA: all data is live per-session cluster state,
			// nothing to prerender. Builds to ./build, embedded by cmd/stornas.
			adapter: adapter({
				fallback: 'index.html',
				precompress: false
			})
		})
	],
	server: {
		// Dev: proxy /api to the Go backend so the SPA can use same-origin paths.
		// ws:true upgrades the WebSocket stream through the proxy too.
		proxy: {
			'/api': { target: 'http://localhost:8080', ws: true }
		}
	}
});
