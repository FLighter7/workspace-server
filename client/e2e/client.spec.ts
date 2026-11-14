import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Browser, type Page } from '@playwright/test';
import './harness.js';
import { SCRATCH, Server, buildServer } from './servers.js';

/**
 * The built package against a real workspace-server, in a real browser. The
 * server is built with Go from this repository (or WORKSPACE_SERVER_BIN runs
 * one already built) and keeps its data under the repository's tmp/.
 */

let server: Server | null = null;
let tmp = '';

test.describe.configure({ mode: 'serial' });

/** A fresh one-time pairing code, as the server's page would show it. */
function pairingCode(): Promise<string> {
	if (!server) throw new Error('no server');

	return server.pairingCode();
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

async function openFilesDevice(browser: Browser): Promise<Page> {
	const page = await openPage(browser);
	await page.evaluate(() => {
		window.files = window.harness.createFilesDevice();
	});

	return page;
}

/** The files a files device holds, but the notes devices' `data.json`: the two share the app. */
const filesOf = (page: Page): Promise<Record<string, string>> =>
	page.evaluate(() => {
		const files = window.files.files();
		delete files['data.json'];
		return files;
	});

/** `size` bytes made from `seed`, as the page's files device makes them. */
function generated(size: number, seed: number): Buffer {
	return Buffer.from(Uint8Array.from({ length: size }, (_, i) => (i * 7 + seed) % 251));
}

test.beforeAll(async () => {
	// Building the server with Go takes a while on a cold cache.
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'client-e2e-'));
	server = await Server.start(buildServer(tmp), join(tmp, 'server'));
});

test.afterAll(async () => {
	await server?.stop();
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

test('the protocol client writes, lists, streams, resumes and deletes a document of the set', async ({
	browser,
}) => {
	const page = await openPage(browser);
	// Larger than a channel buffer's worth, so the body goes in many pieces both ways.
	const size = 3 * 1024 * 1024 + 123;

	const outcome = await page.evaluate(
		([code, bytes]) => window.harness.documents('todo', code, bytes),
		[await pairingCode(), size] as const,
	);

	expect(outcome).toEqual({
		before: expect.anything(),
		began: 0,
		received: size,
		committed: true,
		listedBytes: size,
		listedEtag: true,
		revision: true,
		streamed: true,
		tail: true,
		stale: true,
		conflictReceived: 1,
		refused: false,
		deleted: true,
		removed: true,
		left: false,
	});
});

test('two devices sync a set of several documents, and a delete reaches the other', async ({
	browser,
}) => {
	const a = await openFilesDevice(browser);
	const b = await openFilesDevice(browser);
	expect(await a.evaluate((code) => window.files.pair(code), await pairingCode())).toBe('paired');
	expect(await b.evaluate((code) => window.files.pair(code), await pairingCode())).toBe('paired');

	await a.evaluate(() => {
		window.files.write({ key: { path: 'notes', id: 'a', type: 'md' }, text: 'alpha' });
		window.files.write({ key: { path: 'notes/2026', id: 'b', type: 'md' }, text: 'beta' });
	});
	await b.evaluate(() => {
		window.files.write({ key: { path: 'photos', id: 'c', type: 'jpg' }, text: 'jpeg' });
	});

	const all = { 'notes/a.md': 'alpha', 'notes/2026/b.md': 'beta', 'photos/c.jpg': 'jpeg' };
	await expect.poll(() => filesOf(a), { timeout: 15_000 }).toEqual(all);
	await expect.poll(() => filesOf(b), { timeout: 15_000 }).toEqual(all);
	expect(server?.file('todo', 'notes/2026/b.md')?.toString()).toBe('beta');

	await expect.poll(() => b.evaluate(() => window.files.state.status)).toBe('connected');
	const lists = await b.evaluate(() => window.files.state.lists);
	await b.evaluate(() => window.files.remove({ path: 'notes', id: 'a', type: 'md' }));
	await expect
		.poll(() => filesOf(a), { timeout: 15_000 })
		.toEqual({ 'notes/2026/b.md': 'beta', 'photos/c.jpg': 'jpeg' });
	expect(server?.file('todo', 'notes/a.md')).toBeNull();
	expect(await a.evaluate(() => window.files.state.status)).toBe('connected');
	// The revision the engine computes for its own commit is the server's: one
	// listing, no second sync to find out what the commit's revision holds.
	expect(await b.evaluate(() => window.files.state.lists)).toBe(lists + 1);
	const pairing = JSON.parse((await b.evaluate(() => localStorage.getItem('e2e.files'))) ?? '{}');
	expect(pairing.lastRevision).toBe(server?.revision('todo'));
});

test('a large upload cut off by a reload resumes from what the server received', async ({
	browser,
}) => {
	test.setTimeout(90_000);
	const MiB = 1024 * 1024;
	const big = { key: { path: 'videos', id: 'v1', type: 'mp4' }, size: 8 * MiB, seed: 5 };
	const page = await openFilesDevice(browser);
	expect(await page.evaluate((code) => window.files.pair(code), await pairingCode())).toBe(
		'paired',
	);

	// Each MiB is slow to read, so the upload is caught halfway.
	await page.evaluate((file) => window.files.write({ ...file, delayMs: 250 }), big);
	await expect
		.poll(
			() =>
				page.evaluate(() => {
					const transfer = window.files.state.transfer;
					return transfer?.direction === 'up' ? transfer.done : 0;
				}),
			{ timeout: 30_000, intervals: [50] },
		)
		.toBeGreaterThanOrEqual(3 * MiB);

	// The app reloads mid-upload: the next run finds it in the pairing and waits.
	await page.reload();
	await page.waitForSelector('body[data-ready="true"]', { state: 'attached' });
	await page.evaluate((file) => {
		window.files = window.harness.createFilesDevice([{ ...file, delayMs: 0 }]);
	}, big);
	await expect
		.poll(() => page.evaluate(() => window.files.state.status), { timeout: 30_000 })
		.toBe('interrupted');
	expect(await page.evaluate(() => window.files.state.transfer)).toMatchObject({
		direction: 'up',
		total: 8 * MiB,
		interrupted: true,
	});

	await page.evaluate(() => window.files.resumeTransfer());
	await expect
		.poll(() => page.evaluate(() => window.files.state.status), { timeout: 30_000 })
		.toBe('connected');

	// It went on from where the server stopped, not from the start.
	const transfers = await page.evaluate(() => window.files.state.transfers);
	const resumed = transfers.find((t) => t?.direction === 'up' && !t.interrupted);
	expect(resumed?.done).toBeGreaterThan(0);
	expect(resumed?.done).toBeLessThan(8 * MiB);
	expect(server?.file('todo', 'videos/v1.mp4')?.equals(generated(big.size, big.seed))).toBe(true);
	expect(await page.evaluate(() => localStorage.getItem('e2e.files'))).not.toContain('"upload"');
});
