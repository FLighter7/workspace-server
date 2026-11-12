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
