import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { expectNoCspViolations, nav, openPage } from './harness';
import { buildServer, onLoopback, SCRATCH, Server } from './servers';

/**
 * Switching the main from the page, on two real servers on this machine: the
 * laptop's server, linked to the desk's, becomes the main from its own page;
 * the desk hands the role back from its Devices; and with the desk switched
 * off, the laptop is made the main by force, behind a confirmation that says
 * what happens next.
 */

let desk: Server | null = null;
let laptop: Server | null = null;
let tmp = '';

test.describe.configure({ mode: 'serial' });

interface Upstream {
	linked: boolean;
	connected: boolean;
	serverId?: string;
	term: number;
}

test.beforeAll(async () => {
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'page-e2e-switch-'));
	const binary = buildServer(tmp);
	mkdirSync(join(tmp, 'desk', 'apps', 'todo'), { recursive: true });
	writeFileSync(join(tmp, 'desk', 'apps', 'todo', 'data.json'), '{"v":1}');
	desk = await Server.start(binary, join(tmp, 'desk'));
	laptop = await Server.start(binary, join(tmp, 'laptop'));
	await desk.api('PUT', '/api/config', { name: 'Desk' });
	await laptop.api('PUT', '/api/config', { name: 'Laptop' });
	const { code } = await desk.api<{ code: string }>('POST', '/api/pairing');
	await laptop.api('POST', '/api/upstream', { code: onLoopback(code) });
	await expect
		.poll(async () => (await laptop?.api<Upstream>('GET', '/api/upstream'))?.connected, {
			timeout: 20_000,
		})
		.toBe(true);
	await expect
		.poll(async () => laptop?.api('GET', '/api/apps/todo/document').catch(() => null), {
			timeout: 20_000,
		})
		.toEqual({ v: 1 });
});

test.afterAll(async () => {
	await laptop?.stop();
	await desk?.stop();
	rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
});

function servers(): { desk: Server; laptop: Server } {
	if (!desk || !laptop) throw new Error('the servers did not start');
	return { desk, laptop };
}

const upstream = (page: Page) => page.getByRole('region', { name: 'Upstream server' });
const said = (page: Page) => upstream(page).getByRole('status').last();
const fact = (page: Page, label: string) =>
	upstream(page).locator(`div:has(> dt:text-is("${label}")) > dd`);

async function serverId(server: Server): Promise<string> {
	return (await server.api<{ serverId: string }>('GET', '/api/status')).serverId;
}

/** Whether `server` follows `main`, connected, at `term`. */
async function follows(server: Server, main: Server, term: number): Promise<boolean> {
	const st = await server.api<Upstream>('GET', '/api/upstream');
	return st.connected && st.serverId === (await serverId(main)) && st.term === term;
}

test('the laptop becomes the main from its page, after a question; the desk follows', async ({
	browser,
}) => {
	test.setTimeout(90_000);
	const { desk, laptop } = servers();
	const page = await openPage(browser, laptop.base, '#server');
	const section = upstream(page);
	const make = section.getByRole('button', { name: 'Make this server the main' });

	await make.click();
	const dialog = page.getByRole('dialog', { name: 'Make this server the main?' });
	await expect(dialog).toContainText('The documents are made the same on both servers first.');
	await expect(dialog).toContainText('Devices follow by themselves, without pairing again.');
	await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
	await dialog.getByRole('button', { name: 'Cancel' }).click();
	await expect(dialog).toBeHidden();
	await expect(make).toBeFocused();

	await make.click();
	await dialog.getByRole('button', { name: 'Make it the main' }).click();
	await expect(said(page)).toHaveText('This server is the main now, and Desk syncs through it.', {
		timeout: 45_000,
	});
	await expect(section.getByRole('heading', { level: 2, name: 'Upstream server' })).toBeFocused();
	await expect(section).toContainText('This server is a main');
	await expect.poll(() => follows(desk, laptop, 1), { timeout: 20_000 }).toBe(true);
	expect(await desk.api('GET', '/api/apps/todo/document')).toEqual({ v: 1 });

	// The desk's page shows the laptop as its main now.
	const deskPage = await openPage(browser, desk.base, '#server');
	await expect(fact(deskPage, 'Main server')).toHaveText(`Laptop ${await serverId(laptop)}`);
	await expect(fact(deskPage, 'State')).toHaveText('Connected', { timeout: 20_000 });

	await expectNoCspViolations(page);
	await expectNoCspViolations(deskPage);
	await deskPage.context().close();
	await page.context().close();
});

test('the laptop hands the role back to the desk from its Devices', async ({ browser }) => {
	test.setTimeout(90_000);
	const { desk, laptop } = servers();
	const page = await openPage(browser, laptop.base, '#devices');
	const card = page.getByRole('listitem').filter({
		has: page.getByRole('heading', { level: 3, name: 'Desk' }),
	});
	await expect(card).toContainText('Connected now', { timeout: 20_000 });

	await card.getByRole('button', { name: 'Make it the main' }).click();
	const dialog = page.getByRole('dialog', { name: 'Make Desk the main?' });
	await expect(dialog).toContainText('this one hands its role over and syncs through it');
	await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
	await dialog.getByRole('button', { name: 'Make it the main' }).click();
	await expect(page.getByRole('status').first()).toHaveText(
		'Desk is the main now; this server syncs through it.',
		{ timeout: 45_000 },
	);
	// Now a subordinate, this server pairs nothing: devices pair with the desk.
	await expect(page.getByText('This server is linked to the main server Desk')).toBeVisible();
	await expect.poll(() => follows(laptop, desk, 2), { timeout: 20_000 }).toBe(true);

	await expectNoCspViolations(page);
	await page.context().close();
});

test('with the desk switched off, the laptop is made the main by force, behind a confirmation that says what happens next', async ({
	browser,
}) => {
	test.setTimeout(90_000);
	const { desk, laptop } = servers();
	await desk.stop();
	const page = await openPage(browser, laptop.base, '#server');
	const section = upstream(page);
	await expect(fact(page, 'State')).toHaveText('Not connected', { timeout: 30_000 });

	await section.getByRole('button', { name: 'Make this server the main' }).click();
	const dialog = page.getByRole('dialog', {
		name: 'Desk is out of reach. Make this server the main anyway?',
	});
	await expect(dialog).toContainText(
		'If the main still runs, both act as main until they meet: devices sync with whichever one they reach, and nothing is lost.',
	);
	await expect(dialog).toContainText(
		'When the two meet, the one made main later wins: the old main steps down by itself and syncs through this server.',
	);
	await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
	await dialog.getByRole('button', { name: 'Make it the main anyway' }).click();

	await expect(said(page)).toHaveText(
		'This server is the main now. The old main steps down when the two meet; the other servers follow when this server reaches them.',
		{ timeout: 30_000 },
	);
	await expect(section).toContainText('This server took over as the main.');
	await expect(section.getByRole('listitem').filter({ hasText: 'Desk' })).toBeVisible();
	const st = await laptop.api<Upstream>('GET', '/api/upstream');
	expect(st).toMatchObject({ linked: false, term: 3 });

	// The desk stays listed, so that it can be removed if it is gone for good.
	await nav(page).getByRole('link', { name: 'Devices' }).click();
	await expect(
		page
			.getByRole('listitem')
			.filter({ has: page.getByRole('heading', { level: 3, name: 'Desk' }) }),
	).toContainText('Not connected');

	await expectNoCspViolations(page);
	await page.context().close();
});
