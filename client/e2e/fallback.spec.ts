import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Browser, type Page } from '@playwright/test';
import './harness.js';
import { SCRATCH, Server, buildServer, onLoopback } from './servers.js';

/**
 * The built package against a main and its subordinate, two real servers
 * on this machine: a device pairs with the main only, learns the
 * subordinate from the main's welcome, syncs through it while the main is
 * away, and every copy converges once they meet again.
 */

let tmp = '';
let main: Server;
let laptop: Server;

test.describe.configure({ mode: 'serial' });

async function openDevice(browser: Browser, notes: string[]): Promise<Page> {
	const page = await (await browser.newContext()).newPage();
	await page.goto('/e2e/page/index.html');
	await page.waitForSelector('body[data-ready="true"]', { state: 'attached' });
	await page.evaluate((initial) => {
		window.device = window.harness.createDevice(initial);
	}, notes);

	return page;
}

const notesOf = (page: Page): Promise<string[]> => page.evaluate(() => window.device.state.notes);
const viaOf = (page: Page): Promise<string | null> => page.evaluate(() => window.device.state.via);
const reconnect = (page: Page): Promise<void> => page.evaluate(() => window.device.reconnect());
const add = (page: Page, note: string): Promise<void> =>
	page.evaluate((n) => window.device.add(n), note);

/** The notes in a server's document, `[]` when it has none. */
async function notesOn(server: Server): Promise<string[]> {
	const doc = await server.document('todo');

	return doc === null ? [] : (JSON.parse(doc) as { notes: string[] }).notes;
}

/** Whether the server is linked to its main and connected. */
async function linked(server: Server): Promise<boolean> {
	return ((await server.api('GET', '/api/upstream')) as { connected: boolean }).connected;
}

test.beforeAll(async () => {
	// Building the server with Go takes a while on a cold cache.
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'client-e2e-hub-'));
	const binary = buildServer(tmp);
	main = await Server.start(binary, join(tmp, 'desk'));
	laptop = await Server.start(binary, join(tmp, 'laptop'));
	await main.api('PUT', '/api/config', { name: 'Desk' });
	await laptop.api('PUT', '/api/config', { name: 'Laptop' });
	// The link goes over loopback, so it does not depend on this machine's LAN.
	await laptop.api('POST', '/api/upstream', { code: onLoopback(await main.pairingCode()) });
	await expect.poll(() => linked(laptop), { timeout: 20_000 }).toBe(true);
});

test.afterAll(async () => {
	await Promise.all([main?.stop(), laptop?.stop()]);
	rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
});

let a: Page;
let b: Page;

test('a device paired with the main learns its subordinate and syncs through it while the main is away', async ({
	browser,
}) => {
	test.setTimeout(120_000);
	a = await openDevice(browser, ['a']);
	b = await openDevice(browser, ['b']);
	expect(await a.evaluate((code) => window.device.pair(code), await main.pairingCode())).toBe(
		'paired',
	);
	expect(await b.evaluate((code) => window.device.pair(code), await main.pairingCode())).toBe(
		'paired',
	);
	const pairing = JSON.parse((await a.evaluate(() => localStorage.getItem('e2e.server'))) ?? '{}');
	expect(pairing.fallbacks.map((server: { name: string }) => server.name)).toEqual(['Laptop']);
	await expect.poll(() => notesOf(a), { timeout: 15_000 }).toEqual(['a', 'b']);
	await expect.poll(() => notesOn(laptop), { timeout: 15_000 }).toEqual(['a', 'b']);

	// The main goes away: the device syncs through the laptop.
	await main.stop();
	await reconnect(a);
	await expect.poll(() => viaOf(a), { timeout: 30_000 }).toBe('Laptop');
	expect(await a.evaluate(() => window.device.state.status)).toBe('connected');
	await add(a, 'away');
	await expect.poll(() => notesOn(laptop), { timeout: 15_000 }).toEqual(['a', 'away', 'b']);

	// The main comes back: the laptop pushes what it got meanwhile, and a
	// device reconnecting prefers the main again.
	await main.run();
	await expect.poll(() => notesOn(main), { timeout: 30_000 }).toEqual(['a', 'away', 'b']);
	await reconnect(b);
	await expect.poll(() => notesOf(b), { timeout: 30_000 }).toEqual(['a', 'away', 'b']);
	expect(await viaOf(b)).toBeNull();
});

test('edits on both sides while the two servers cannot meet converge', async () => {
	test.setTimeout(120_000);
	// The laptop's side moves while the main is away...
	await main.stop();
	await reconnect(a);
	await expect.poll(() => viaOf(a), { timeout: 30_000 }).toBe('Laptop');
	await add(a, 'laptop-side');
	await expect.poll(() => notesOn(laptop), { timeout: 15_000 }).toContain('laptop-side');

	// ...and the main's while the laptop is away.
	await laptop.stop();
	await main.run();
	await reconnect(b);
	await expect
		.poll(() => b.evaluate(() => window.device.state.status), { timeout: 30_000 })
		.toBe('connected');
	await add(b, 'desk-side');
	await expect.poll(() => notesOn(main), { timeout: 15_000 }).toContain('desk-side');

	// They meet: the main's copy wins on the laptop, whose own goes to its
	// history, and the devices merge what each holds.
	await laptop.run();
	await expect.poll(() => linked(laptop), { timeout: 20_000 }).toBe(true);
	await reconnect(a);
	const all = ['a', 'away', 'b', 'desk-side', 'laptop-side'];
	await expect.poll(() => notesOn(main), { timeout: 30_000 }).toEqual(all);
	await expect.poll(() => notesOn(laptop), { timeout: 30_000 }).toEqual(all);
	await expect.poll(() => notesOf(a), { timeout: 15_000 }).toEqual(all);
	await expect.poll(() => notesOf(b), { timeout: 15_000 }).toEqual(all);

	const history = join(laptop.dir, 'apps', 'todo', 'history');
	const kept = readdirSync(history).map((name) => readFileSync(join(history, name), 'utf8'));
	expect(kept.some((doc) => doc.includes('laptop-side') && !doc.includes('desk-side'))).toBe(true);
});
