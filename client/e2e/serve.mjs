// Serves the built package and the e2e page to the browser: dist/ is plain
// ESM with relative imports, so no bundler is needed. Usage: node serve.mjs <port>
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, resolve, sep } from 'node:path';

const root = resolve(import.meta.dirname, '..');
const served = [resolve(root, 'dist'), resolve(root, 'e2e', 'page')];
const types = {
	'.html': 'text/html; charset=utf-8',
	'.js': 'text/javascript; charset=utf-8',
	'.map': 'application/json; charset=utf-8',
};
const port = Number(process.argv[2] ?? 4180);

createServer(async (request, response) => {
	const path = decodeURIComponent(new URL(request.url ?? '/', 'http://localhost').pathname);
	const file = resolve(root, `.${path}`);
	if (!served.some((dir) => file.startsWith(dir + sep))) {
		response.writeHead(404).end();
		return;
	}
	try {
		const body = await readFile(file);
		response.writeHead(200, { 'content-type': types[extname(file)] ?? 'application/octet-stream' });
		response.end(body);
	} catch {
		response.writeHead(404).end();
	}
}).listen(port, 'localhost');
