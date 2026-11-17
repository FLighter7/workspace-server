import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { expectNoCspViolations, openPage } from './harness';
import { buildServer, onLoopback, SCRATCH, Server } from './servers';

/**
 * The Apps tab on two real servers on this machine, a main ("Desk") and a
 * server linked to it ("Laptop"), whose copies of todo differ before the
 * link: both pages mark the divergence by the other server's name; the
 * quota is changed on the main's page, and the laptop follows it, read-only.
 * Opening a folder is answered by a stand-in in the browser, so no window
 * opens; the server's side of it is tested in Go.
 */

const MB = 1024 * 1024;

/** Where the screenshots of the new screens go, for the stage's report (gitignored). */
const SHOTS = join(SCRATCH, 'reports', 'files-stage4');

let main: Server | null = null;
let sub: Server | null = null;
let tmp = '';

test.describe.configure({ mode: 'serial' });

interface App {
	id: string;
	quota: number;
	diverged: { serverId: string; name: string }[];
}

interface Status {
	serverId: string;
}

/** Writes todo's data.json into a server folder's WSS-Data before it starts. */
function seed(dir: string, body: string): void {
	mkdirSync(join(dir, 'WSS-Data', 'apps', 'todo'), { recursive: true });
	writeFileSync(join(dir, 'WSS-Data', 'apps', 'todo', 'data.json'), body);
}

test.beforeAll(async () => {
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	mkdirSync(SHOTS, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'page-e2e-apps-'));
	const binary = buildServer(tmp);
	seed(join(tmp, 'main'), '{"desk":1}');
	seed(join(tmp, 'sub'), '{"laptop":1}');
	main = await Server.start(binary, join(tmp, 'main'));
	sub = await Server.start(binary, join(tmp, 'sub'));
	await main.api('PUT', '/api/config', { name: 'Desk' });
	await sub.api('PUT', '/api/config', { name: 'Laptop' });
	await sub.api('POST', '/api/upstream', { code: onLoopback((await main.pairing()).code) });
	// The first link finds two different sets: the copies have diverged.
	await expect
		.poll(async () => (await sub?.api<{ diverged?: string[] }>('GET', '/api/upstream'))?.diverged, {
			timeout: 20_000,
		})
		.toEqual(['todo']);
});

test.afterAll(async () => {
	await sub?.stop();
	await main?.stop();
	rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
});

function servers(): { main: Server; sub: Server } {
	if (!main || !sub) throw new Error('the servers did not start');
	return { main, sub };
}

/** The card of todo on the Apps tab. */
const todoCard = (page: Page) =>
	page.getByRole('listitem').filter({ has: page.getByRole('heading', { name: 'Todo todo' }) });

/** The section's status line. */
const said = (page: Page) => page.getByRole('region', { name: 'Apps' }).getByRole('status').first();

test('both pages mark the divergence by the other server, and say to choose on a device', async ({
	browser,
}) => {
	const { main, sub } = servers();
	const mainStatus = await main.api<Status>('GET', '/api/status');
	const subStatus = await sub.api<Status>('GET', '/api/status');

	const laptop = await openPage(browser, sub.base, '#apps');
	const card = todoCard(laptop);
	await expect(card).toContainText('Diverged: choose on a device');
	await expect(card).toContainText("The app's files here and on Desk differ.");
	const apps = await sub.api<App[]>('GET', '/api/apps');
	expect(apps.find((app) => app.id === 'todo')?.diverged).toEqual([
		expect.objectContaining({ serverId: mainStatus.serverId, name: 'Desk' }),
	]);
	await laptop.screenshot({ path: join(SHOTS, 'apps-subordinate-diverged.png'), fullPage: true });
	await expectNoCspViolations(laptop);
	await laptop.context().close();

	// The main hears of it once the laptop tells it, and polls it in.
	const desk = await openPage(browser, main.base, '#apps');
	await expect(todoCard(desk)).toContainText("The app's files here and on Laptop differ.", {
		timeout: 20_000,
	});
	const mainApps = await main.api<App[]>('GET', '/api/apps');
	expect(mainApps.find((app) => app.id === 'todo')?.diverged).toEqual([
		expect.objectContaining({ serverId: subStatus.serverId, name: 'Laptop' }),
	]);
	await expectNoCspViolations(desk);
	await desk.context().close();
});

test("the quota is changed on the main's page, and the laptop follows it, read-only", async ({
	browser,
}) => {
	const { main, sub } = servers();

	// The laptop's API refuses: its quotas are the main's.
	await expect(sub.api('PUT', '/api/apps/todo', { quota: 3 * MB })).rejects.toThrow(
		/409.*followsMain/,
	);

	const desk = await openPage(browser, main.base, '#apps');
	const card = todoCard(desk);
	await expect(card).toContainText('of 50 MB');
	await expect(
		card.getByRole('progressbar', { name: 'Space used of the quota of Todo' }),
	).toBeVisible();
	const change = card.getByRole('button', { name: 'Change the quota of Todo' });
	await change.click();
	const dialog = desk.getByRole('dialog', { name: 'The quota of Todo' });
	const field = dialog.getByLabel('Maximum storage size', { exact: true });
	await expect(field).toBeFocused();
	await expect(field).toHaveValue('50');
	await field.fill('0');
	await dialog.getByRole('button', { name: 'Save' }).click();
	// The top is what the disk has free, which is the machine's.
	await expect(dialog).toContainText(/Enter a whole number from 1 to [\d,]+\./);
	await field.fill('3');
	await desk.screenshot({ path: join(SHOTS, 'apps-quota-dialog.png') });
	await dialog.getByRole('button', { name: 'Save' }).click();
	await expect(dialog).toBeHidden();
	await expect(said(desk)).toHaveText('The quota of Todo is now 3 MB.');
	await expect(change).toBeFocused();
	await expect(card).toContainText('of 3 MB');
	const mainApps = await main.api<App[]>('GET', '/api/apps');
	expect(mainApps.find((app) => app.id === 'todo')?.quota).toBe(3 * MB);
	await expectNoCspViolations(desk);
	await desk.context().close();

	await expect
		.poll(
			async () =>
				(await sub.api<App[]>('GET', '/api/apps')).find((app) => app.id === 'todo')?.quota,
			{ timeout: 20_000 },
		)
		.toBe(3 * MB);
	// An app only the laptop serves keeps a quota of its own.
	await sub.api('POST', '/api/apps', { id: 'photos', name: 'Photos' });
	const laptop = await openPage(browser, sub.base, '#apps');
	const subCard = todoCard(laptop);
	await expect(laptop.getByRole('button', { name: 'Change the quota of Photos' })).toBeVisible();
	await expect(subCard).toContainText('of 3 MB');
	await expect(subCard).toContainText(
		"The main server, Desk, sets the quota: change it on the main's page.",
	);
	await expect(subCard.getByRole('button', { name: /Change the quota/ })).toHaveCount(0);
	// A new app takes a quota of its own, until the main serves it too.
	await laptop.getByRole('button', { name: 'Add an app' }).click();
	await expect(laptop.getByLabel('Maximum storage size', { exact: true })).toHaveValue('50');
	await expect(laptop.getByText("the main's quota replaces it")).toBeVisible();
	await expectNoCspViolations(laptop);
	await laptop.context().close();
});

test("Open folder asks the server to open the app's folder", async ({ browser }) => {
	const { main } = servers();
	const page = await openPage(browser, main.base, '#apps');
	// A stand-in answers in place of the server, which would open a window.
	const asked: string[] = [];
	await page.route('**/api/apps/*/open', async (route) => {
		const request = route.request();
		asked.push(`${request.method()} ${new URL(request.url()).pathname}`);
		expect(request.headers()['x-workspace-server']).toBe('1');
		await route.fulfill({ status: 204 });
	});

	const card = todoCard(page);
	await expect(card).toContainText(join('WSS-Data', 'apps', 'todo'));
	await card.getByRole('button', { name: 'Open folder of Todo' }).click();
	await expect(said(page)).toHaveText('The folder of Todo opens in the file manager.');
	expect(asked).toEqual(['POST /api/apps/todo/open']);

	// The server's own answer for an app it does not serve: nothing opens.
	await expect(main.api('POST', '/api/apps/none/open')).rejects.toThrow(/404.*notFound/);

	// A phone's width: the card's facts stack, and nothing scrolls sideways.
	await page.setViewportSize({ width: 360, height: 800 });
	await expect(card).toBeVisible();
	expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
		true,
	);
	await page.screenshot({ path: join(SHOTS, 'apps-main-phone.png'), fullPage: true });

	await expectNoCspViolations(page);
	await page.context().close();
});
