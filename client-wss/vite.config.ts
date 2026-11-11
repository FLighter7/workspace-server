import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vitest/config';
import vue from '@vitejs/plugin-vue';
import vuetify from 'vite-plugin-vuetify';
import vueI18n from '@intlify/unplugin-vue-i18n/vite';

/**
 * The server `pnpm run dev` sends /api to: a throwaway one, with a data folder
 * under the repository's tmp/ and ports 47810 (the page) and 47811 (UDP), as
 * .claude/CLAUDE.md asks.
 */
const target = process.env.WSS_TARGET ?? 'http://127.0.0.1:47810';

export default defineConfig({
	plugins: [
		vue(),
		vuetify({ autoImport: true }),
		vueI18n({
			include: [fileURLToPath(new URL('./src/i18n/locales/*.json', import.meta.url))],
			// Precompiled messages and no compiler at run time: the compiler needs
			// eval, which the page's CSP forbids.
			runtimeOnly: true,
		}),
	],
	resolve: {
		alias: {
			'@': fileURLToPath(new URL('./src', import.meta.url)),
		},
	},
	build: {
		// Embedded by internal/web with //go:embed; never committed.
		outDir: fileURLToPath(new URL('../internal/web/ui/dist', import.meta.url)),
		emptyOutDir: true,
	},
	server: {
		port: 5180,
		proxy: {
			'/api': {
				target,
				changeOrigin: true,
				configure(proxy) {
					// The server's guard takes requests from its own page only: Host is
					// rewritten by changeOrigin, Origin here. The server never sends
					// CORS headers, and must not start to for this.
					proxy.on('proxyReq', (request) => {
						if (request.getHeader('origin')) request.setHeader('origin', target);
					});
				},
			},
		},
	},
	test: {
		environment: 'node',
		include: ['src/**/*.test.ts'],
		setupFiles: ['src/vitest.setup.ts'],
		server: {
			deps: {
				// vite-plugin-vuetify adds a .css import per component, which Node's
				// own loader cannot read; inlined, Vite strips them.
				inline: ['vuetify'],
			},
		},
	},
});
