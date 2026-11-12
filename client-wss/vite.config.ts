import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath, URL } from 'node:url';
import type { Plugin } from 'vite';
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

/**
 * qr-scanner starts its decoder as a Worker made from a blob: URL, which the
 * page's CSP refuses: a worker's source falls back to default-src 'self'. In
 * the build, its worker module is replaced by one that starts the same code
 * from a file of the page's own, under /assets/, which the CSP allows. The
 * code is taken from the package's own module at build time, by running it
 * with stand-ins for Worker, URL and Blob, so it follows the package's
 * updates. The dev server sends no CSP and keeps the blob.
 */
function qrScannerWorkerAsFile(): Plugin {
	const WORKER = /[\\/]qr-scanner[\\/]qr-scanner-worker\.min\.js$/;

	return {
		name: 'qr-scanner-worker-as-file',
		apply: 'build',
		load(id) {
			if (!WORKER.test(id)) return null;

			// export const createWorker=()=>new Worker(URL.createObjectURL(new Blob([`…`],…)))
			const module = readFileSync(id, 'utf8')
				.replace(/\/\/# sourceMappingURL=\S*\s*$/, '')
				.replace(/^export const createWorker\s*=/, 'return ');
			let source = '';
			class Blob {
				constructor(parts: string[]) {
					source = parts.join('');
				}
			}
			const createWorker = new Function('Worker', 'URL', 'Blob', module)(
				class {},
				{ createObjectURL: () => '' },
				Blob,
			) as () => void;
			createWorker();
			if (!source.includes('onmessage')) {
				throw new Error(`qr-scanner's worker module changed shape: ${id}`);
			}

			const hash = createHash('sha256').update(source).digest('hex').slice(0, 8);
			const fileName = `assets/qr-scanner-worker-${hash}.js`;
			this.emitFile({ type: 'asset', fileName, source });
			return `export const createWorker = () => new Worker(${JSON.stringify(`/${fileName}`)});`;
		},
	};
}

export default defineConfig({
	plugins: [
		qrScannerWorkerAsFile(),
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
