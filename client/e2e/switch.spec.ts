import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Browser, type Page } from '@playwright/test';
import './harness.js';
import { SCRATCH, Server, buildServer, onLoopback } from './servers.js';

/**
 * The built package across switches of the main, against three real
 * servers on this machine: a device paired with the desk, the first main,
 * keeps syncing when the laptop takes the role over in a planned switch,
 * and again when the tablet takes it over by force with the laptop gone
 * for good. It never pairs again.
 */

let tmp = '';
let desk: Server;
let laptop: Server;
let tablet: Server;

test.describe.configure({ mode: 'serial' });

interface Upstream {
	linked: boolean;
	connected: boolean;
	serverId?: string;
	term: number;
}

const upstreamOf = (server: Server): Promise<Upstream> =>
	server.api('GET', '/api/upstream') as Promise<Upstream>;

async function serverIdOf(server: Server): Promise<string> {
	return ((await server.api('GET', '/api/status')) as { serverId: string }).serverId;
}

/** The notes in a server's document, `[]` when it has none. */
async function notesOn(server: Server): Promise<string[]> {
	const doc = await server.document('todo');

	return doc === null ? [] : (JSON.parse(doc) as { notes: string[] }).notes;
}

/** Whether `server` follows the main `main`, connected, at `term`. */
async function follows(server: Server, main: Server, term: number): Promise<boolean> {
	const st = await upstreamOf(server);

	return st.connected && st.serverId === (await serverIdOf(main)) && st.term === term;
}

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
const reconnect = (page: Page): Promise<void> => page.evaluate(() => window.device.reconnect());
const add = (page: Page, note: string): Promise<void> =>
	page.evaluate((n) => window.device.add(n), note);
const pairingOf = async (
	page: Page,
): Promise<{ serverId: string; term?: number; device: string; fallbacks?: { name: string }[] }> =>
	JSON.parse((await page.evaluate(() => localStorage.getItem('e2e.server'))) ?? '{}');

test.beforeAll(async () => {
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'client-e2e-switch-'));
	const binary = buildServer(tmp);
	desk = await Server.start(binary, join(tmp, 'desk'));
	laptop = await Server.start(binary, join(tmp, 'laptop'));
	tablet = await Server.start(binary, join(tmp, 'tablet'));
	await desk.api('PUT', '/api/config', { name: 'Desk' });
	await laptop.api('PUT', '/api/config', { name: 'Laptop' });
	await tablet.api('PUT', '/api/config', { name: 'Tablet' });
	for (const sub of [laptop, tablet]) {
		await sub.api('POST', '/api/upstream', { code: onLoopback(await desk.pairingCode()) });
		await expect
			.poll(async () => (await upstreamOf(sub)).connected, { timeout: 20_000 })
			.toBe(true);
	}
});

test.afterAll(async () => {
	await Promise.all([desk?.stop(), laptop?.stop(), tablet?.stop()]);
	rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
});

let a: Page;

test('a device paired with the old main keeps syncing across a planned switch', async ({
	browser,
}) => {
	test.setTimeout(150_000);
	a = await openDevice(browser, ['a']);
	expect(await a.evaluate((code) => window.device.pair(code), await desk.pairingCode())).toBe(
		'paired',
	);
	const paired = await pairingOf(a);
	expect(paired.serverId).toBe(await serverIdOf(desk));
	await expect.poll(() => notesOn(laptop), { timeout: 20_000 }).toEqual(['a']);
	await expect.poll(() => notesOn(tablet), { timeout: 20_000 }).toEqual(['a']);
	// The device knows both subordinates before the switch.
	await expect
		.poll(async () => ((await pairingOf(a)).fallbacks ?? []).map((s) => s.name).sort())
		.toEqual(['Laptop', 'Tablet']);

	// "Make this server the main" on the laptop.
	expect(await laptop.api('POST', '/api/upstream/promote', {})).toEqual({ followed: true });
	await expect.poll(() => follows(desk, laptop, 1), { timeout: 30_000 }).toBe(true);
	await expect.poll(() => follows(tablet, laptop, 1), { timeout: 30_000 }).toBe(true);

	// The device follows the new main without pairing again.
	await reconnect(a);
	await expect
		.poll(async () => (await pairingOf(a)).serverId, { timeout: 30_000 })
		.toBe(await serverIdOf(laptop));
	expect(await pairingOf(a)).toMatchObject({ device: paired.device, term: 1 });
	expect(await a.evaluate(() => window.device.state.via)).toBeNull();
	await add(a, 'after-planned');
	for (const server of [laptop, desk, tablet]) {
		await expect.poll(() => notesOn(server), { timeout: 30_000 }).toEqual(['a', 'after-planned']);
	}
});

test('and after a forced switch, with the old main gone for good', async () => {
	test.setTimeout(150_000);
	await laptop.stop(); // gone for good
	await expect
		.poll(async () => (await upstreamOf(tablet)).connected, { timeout: 30_000 })
		.toBe(false);

	// "Make this server the main" on the tablet, confirmed while the main is
	// out of reach.
	expect(await tablet.api('POST', '/api/upstream/promote', { force: true })).toEqual({
		followed: false,
	});
	await expect.poll(() => follows(desk, tablet, 2), { timeout: 60_000 }).toBe(true);

	await reconnect(a);
	await expect
		.poll(async () => (await pairingOf(a)).serverId, { timeout: 60_000 })
		.toBe(await serverIdOf(tablet));
	expect((await pairingOf(a)).term).toBe(2);
	await add(a, 'after-forced');
	for (const server of [tablet, desk]) {
		await expect
			.poll(() => notesOn(server), { timeout: 30_000 })
			.toEqual(['a', 'after-forced', 'after-planned']);
	}
	expect(await notesOf(a)).toEqual(['a', 'after-forced', 'after-planned']);
});
