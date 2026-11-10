import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createSocket } from 'node:dgram';
import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { join, resolve } from 'node:path';
import { expect, test, type Browser, type Page } from '@playwright/test';

/**
 * The built package against a real workspace-server, in a real browser. The
 * server is built with Go from this repository (or WORKSPACE_SERVER_BIN runs
 * one already built) and keeps its data under the repository's dist/.
 */

interface Device {
	state: { notes: string[]; status: string; error: string | null };
	pair(code: string): Promise<string>;
	add(note: string): void;
}

declare global {
	interface Window {
		harness: {
			createDevice(notes: string[]): Device;
			pairAs(app: string, code: string): Promise<string>;
		};
		device: Device;
	}
}

const REPO = resolve(import.meta.dirname, '..', '..');

let server: ChildProcess | null = null;
let tmp = '';
let httpPort = 0;

test.describe.configure({ mode: 'serial' });

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

function buildServer(): string {
	if (process.env.WORKSPACE_SERVER_BIN) return resolve(process.env.WORKSPACE_SERVER_BIN);

	const binary = join(
		tmp,
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

async function waitForServer(): Promise<void> {
	const deadline = Date.now() + 30_000;
	while (Date.now() < deadline) {
		if (server?.exitCode !== null) throw new Error('workspace-server exited at start');
		try {
			const response = await fetch(`http://127.0.0.1:${httpPort}/api/status`);
			if (response.ok) return;
		} catch {
			// Not listening yet.
		}
		await new Promise((done) => setTimeout(done, 200));
	}

	throw new Error('workspace-server did not start within 30 s');
}

/** A fresh one-time pairing code, as the server's page would show it. */
async function pairingCode(): Promise<string> {
	const response = await fetch(`http://127.0.0.1:${httpPort}/api/pairing`, {
		method: 'POST',
		headers: { 'X-Workspace-Server': '1' },
	});
	expect(response.ok).toBe(true);
	const { code } = (await response.json()) as { code: string };

	return code;
}

async function openPage(browser: Browser): Promise<Page> {
	const context = await browser.newContext();
	const page = await context.newPage();
	await page.goto('/e2e/page/index.html');
	await page.waitForSelector('body[data-ready="true"]', { state: 'attached' });

	return page;
}

async function openDevice(browser: Browser, notes: string[]): Promise<Page> {
	const page = await openPage(browser);
	await page.evaluate((initial) => {
		window.device = window.harness.createDevice(initial);
	}, notes);

	return page;
}

const notesOf = (page: Page): Promise<string[]> => page.evaluate(() => window.device.state.notes);

test.beforeAll(async () => {
	// Building the server with Go takes a while on a cold cache.
	test.setTimeout(180_000);
	const dist = join(REPO, 'dist');
	mkdirSync(dist, { recursive: true });
	tmp = mkdtempSync(join(dist, 'client-e2e-'));
	const binary = buildServer();
	httpPort = await freeTcpPort();
	const udpPort = await freeUdpPort();

	server = spawn(
		binary,
		[
			'run',
			'-dir',
			join(tmp, 'server'),
			'-udp-port',
			String(udpPort),
			'-http-port',
			String(httpPort),
		],
		{ stdio: 'ignore' },
	);
	await waitForServer();
});

test.afterAll(async () => {
	if (server && server.exitCode === null) {
		// Windows releases the server's files only once the process is gone.
		const exited = new Promise((done) => server?.once('exit', done));
		server.kill();
		await exited;
	}
	server = null;
	rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
});

let usedCode = '';

test('two devices pair, merge, and an edit reaches the other through a push', async ({
	browser,
}) => {
	const a = await openDevice(browser, ['a']);
	const b = await openDevice(browser, ['b']);

	usedCode = await pairingCode();
	expect(await a.evaluate((code) => window.device.pair(code), usedCode)).toBe('paired');
	expect(await b.evaluate((code) => window.device.pair(code), await pairingCode())).toBe('paired');

	// b merged what a wrote, and wrote the union; a hears the push and takes it.
	await expect.poll(() => notesOf(b), { timeout: 15_000 }).toEqual(['a', 'b']);
	await expect.poll(() => notesOf(a), { timeout: 15_000 }).toEqual(['a', 'b']);

	await a.evaluate(() => window.device.add('c'));
	await expect.poll(() => notesOf(b), { timeout: 15_000 }).toEqual(['a', 'b', 'c']);
	expect(await b.evaluate(() => window.device.state.status)).toBe('connected');
});

test('a used pairing code is refused with badSecret', async ({ browser }) => {
	const c = await openDevice(browser, []);

	expect(await c.evaluate((code) => window.device.pair(code), usedCode)).toBe('badSecret');
});

test('an app the server does not serve gets unknownApp', async ({ browser }) => {
	const page = await openPage(browser);

	const outcome = await page.evaluate(
		(code) => window.harness.pairAs('notes', code),
		await pairingCode(),
	);

	expect(outcome).toBe('unknownApp');
});
