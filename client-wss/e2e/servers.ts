import { execFile, spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createSocket } from 'node:dgram';
import { existsSync } from 'node:fs';
import { createServer } from 'node:net';
import { join, resolve } from 'node:path';

/**
 * Real workspace-servers for the e2e tests: built with Go from this
 * repository, embedding internal/web/ui/dist (which `pnpm run test:e2e`
 * builds first), or WORKSPACE_SERVER_BIN runs one already built. Each has its
 * own folder under the repository's tmp/ and its own ports.
 */

const REPO = resolve(import.meta.dirname, '..', '..');

/** The repository's tmp/ folder, where the e2e data goes. */
export const SCRATCH = join(REPO, 'tmp');

function freeTcpPort(): Promise<number> {
	return new Promise((resolvePort, reject) => {
		const probe = createServer();
		probe.once('error', reject);
		probe.listen(0, '127.0.0.1', () => {
			const address = probe.address();
			probe.close(() => resolvePort(typeof address === 'object' && address ? address.port : 0));
		});
	});
}

function freeUdpPort(): Promise<number> {
	return new Promise((resolvePort, reject) => {
		const probe = createSocket('udp4');
		probe.once('error', reject);
		probe.bind(0, () => {
			const { port } = probe.address();
			probe.close(() => resolvePort(port));
		});
	});
}

/** Builds the server into `dir`, unless WORKSPACE_SERVER_BIN names one; returns the binary. */
export function buildServer(dir: string): string {
	if (process.env.WORKSPACE_SERVER_BIN) return resolve(process.env.WORKSPACE_SERVER_BIN);
	if (!existsSync(join(REPO, 'internal', 'web', 'ui', 'dist', 'index.html'))) {
		throw new Error('the page is not built: run `pnpm run build` first');
	}

	const binary = join(
		dir,
		process.platform === 'win32' ? 'workspace-server.exe' : 'workspace-server',
	);
	const build = spawnSync('go', ['build', '-o', binary, './cmd/workspace-server'], {
		cwd: REPO,
		encoding: 'utf8',
	});
	if (build.status !== 0) {
		throw new Error(`go build failed: ${build.error?.message ?? ''}${build.stderr ?? ''}`);
	}

	return binary;
}

/** Builds the e2e device (`e2e/device`, Go) into `dir`; returns the binary. */
export function buildDevice(dir: string): string {
	const binary = join(dir, process.platform === 'win32' ? 'device.exe' : 'device');
	const build = spawnSync('go', ['build', '-o', binary, './client-wss/e2e/device'], {
		cwd: REPO,
		encoding: 'utf8',
	});
	if (build.status !== 0) {
		throw new Error(
			`go build of the device failed: ${build.error?.message ?? ''}${build.stderr ?? ''}`,
		);
	}

	return binary;
}

/**
 * Runs the e2e device: it pairs with the main whose code is `code`, then
 * writes `pushed` documents of todo on the main's subordinate, each once the
 * main holds the one before, and `extra` more at once. Resolves to the last
 * document written.
 */
export function runDevice(
	binary: string,
	code: string,
	{ pushed, extra }: { pushed: number; extra: number },
): Promise<{ device: string; last: string }> {
	const args = ['-code', code, '-pushed', String(pushed), '-extra', String(extra)];
	return new Promise((resolveRun, reject) => {
		execFile(binary, args, { encoding: 'utf8' }, (error, stdout, stderr) => {
			if (error) reject(new Error(`the device failed: ${stderr || error.message}`));
			else resolveRun(JSON.parse(stdout));
		});
	});
}

/** One server process on its own folder and ports. */
export class Server {
	private process: ChildProcess | null = null;

	private constructor(
		private readonly binary: string,
		readonly dir: string,
		readonly httpPort: number,
		readonly udpPort: number,
	) {}

	static async start(binary: string, dir: string): Promise<Server> {
		const server = new Server(binary, dir, await freeTcpPort(), await freeUdpPort());
		await server.run();

		return server;
	}

	/** The page's address: localhost, as the program opens it (the browser tries ::1 first and falls back). */
	get base(): string {
		return `http://localhost:${this.httpPort}`;
	}

	get running(): boolean {
		return this.process !== null && this.process.exitCode === null;
	}

	/** Starts the process and waits until its page answers. */
	private async run(): Promise<void> {
		this.process = spawn(
			this.binary,
			[
				'run',
				'-dir',
				this.dir,
				'-udp-port',
				String(this.udpPort),
				'-http-port',
				String(this.httpPort),
			],
			{ stdio: 'ignore' },
		);
		const deadline = Date.now() + 30_000;
		while (Date.now() < deadline) {
			if (this.process.exitCode !== null) throw new Error('workspace-server exited at start');
			try {
				const response = await fetch(`http://127.0.0.1:${this.httpPort}/api/status`);
				if (response.ok) return;
			} catch {
				// Not listening yet.
			}
			await new Promise((done) => setTimeout(done, 200));
		}

		throw new Error('workspace-server did not start within 30 s');
	}

	/** Ends the process and waits until it is gone: Windows releases its files only then. */
	async stop(): Promise<void> {
		const process = this.process;
		this.process = null;
		if (!process || process.exitCode !== null) return;
		const exited = new Promise((done) => process.once('exit', done));
		process.kill();
		await exited;
	}

	/** Calls the page's API; a body is sent as JSON. */
	async api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
		const response = await fetch(`http://127.0.0.1:${this.httpPort}${path}`, {
			method,
			headers: { 'X-Workspace-Server': '1', 'Content-Type': 'application/json' },
			body: body === undefined ? undefined : JSON.stringify(body),
		});
		const text = await response.text();
		if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${text}`);

		return (text === '' ? null : JSON.parse(text)) as T;
	}

	/** A fresh one-time pairing code, as the server's page would show it, with its QR code. */
	pairing(): Promise<{ code: string; qr: string }> {
		return this.api('POST', '/api/pairing');
	}
}

/** The JSON inside a `wsrv1.` pairing code. */
export function decodeCode(code: string): { id: string; n: string; h: string[] } {
	return JSON.parse(Buffer.from(code.slice('wsrv1.'.length), 'base64url').toString('utf8'));
}

/** `code` with 127.0.0.1 as its only host, so that a server on this machine is reached without the LAN. */
export function onLoopback(code: string): string {
	const json = decodeCode(code);

	return (
		'wsrv1.' + Buffer.from(JSON.stringify({ ...json, h: ['127.0.0.1'] })).toString('base64url')
	);
}
