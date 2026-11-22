import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Browser, type BrowserContextOptions } from '@playwright/test';
import { expectNoCspViolations, nav, openPage as open } from './harness';
import { buildServer, SCRATCH, Server } from './servers';

/**
 * The built page in a real workspace-server, in a real browser. Its data
 * stays under the repository's tmp/.
 */

let server: Server | null = null;
let tmp = '';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
	// Building the server with Go takes a while on a cold cache.
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'page-e2e-'));
	server = await Server.start(buildServer(tmp), join(tmp, 'server'));
});

test.afterAll(async () => {
	await server?.stop();
	server = null;
	rmSync(tmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
});

/** A page of the server in a fresh context that records every CSP violation it meets. */
function openPage(browser: Browser, hash = '', options: BrowserContextOptions = {}) {
	return open(browser, server?.base ?? '', hash, options);
}

test('every section opens from the header, and the CSP blocks nothing', async ({ browser }) => {
	const page = await openPage(browser);

	await expect(page.getByRole('heading', { level: 1, name: 'Server' })).toBeVisible();
	await expect(page.getByText('Server ID')).toBeVisible();
	await expect(page.getByLabel('UDP port')).not.toHaveValue('');

	await expect(nav(page).getByRole('link')).toHaveText(['Server', 'Devices', 'Apps']);
	for (const [link, heading] of [
		['Devices', 'Devices'],
		['Apps', 'Apps'],
		['Server', 'Server'],
	]) {
		await nav(page).getByRole('link', { name: link }).click();
		await expect(page.getByRole('heading', { level: 1, name: heading })).toBeVisible();
		await expect(nav(page).getByRole('link', { name: link })).toHaveAttribute(
			'aria-current',
			'page',
		);
	}

	// A dialog, opened and cancelled: the server keeps running.
	await page.getByRole('button', { name: 'Stop the server' }).click();
	const dialog = page.getByRole('dialog', { name: 'Stop the server?' });
	await expect(dialog).toBeVisible();
	await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
	await dialog.getByRole('button', { name: 'Cancel' }).click();
	await expect(dialog).toBeHidden();
	await expect(page.getByRole('button', { name: 'Stop the server' })).toBeFocused();

	await expectNoCspViolations(page);
	await page.context().close();
});

test('an app is added and deleted', async ({ browser }) => {
	const page = await openPage(browser, '#apps');

	// A new server serves todo already.
	await expect(page.getByRole('heading', { level: 2, name: 'Todo', exact: true })).toBeVisible();
	const addButton = page.getByRole('button', { name: 'Add an app' });
	await addButton.click();
	const form = page.getByRole('dialog', { name: 'Add an app' });
	const idField = form.getByRole('textbox', { name: 'ID', exact: true });
	await expect(idField).toBeFocused();
	await expect(idField).toHaveAttribute('required', '');
	await idField.fill('todo');
	await expect(page.getByText('An app with this ID already exists.')).toBeVisible();
	// A capital letter lands as a small one; a character an ID cannot hold never lands.
	await idField.fill('');
	await idField.pressSequentially('Ab_c 1-d');
	await expect(idField).toHaveValue('abc1-d');
	await idField.fill('notes');
	await page.getByLabel('Name', { exact: true }).fill('My notes');
	// The quota in MB, 50 by default; nothing about versions any more.
	await expect(page.getByLabel('Maximum storage size', { exact: true })).toHaveValue('50');
	await page.getByLabel('Maximum storage size', { exact: true }).fill('20');
	await expect(page.getByText(/version/i)).toHaveCount(0);
	await form.getByRole('button', { name: 'Add', exact: true }).click();
	await expect(form).toBeHidden();
	await expect(page.getByRole('status').filter({ hasText: 'Added' })).toBeVisible();
	await expect(addButton).toBeFocused();
	const card = page.getByRole('listitem').filter({
		has: page.getByRole('heading', { name: 'My notes', exact: true }),
	});
	await expect(card).toBeVisible();
	await expect(card).toContainText('notes');
	await expect(card).toContainText('0 bytes of 20 MB');
	// The files are on the unfolded part of the card.
	await expect(card.getByText('No files yet')).toBeHidden();
	await card.getByRole('button', { name: 'Show details of My notes' }).click();
	await expect(card.getByText('No files yet')).toBeVisible();

	await card.getByRole('button', { name: 'Delete', exact: true }).click();
	const dialog = page.getByRole('dialog', { name: 'Delete My notes?' });
	await expect(dialog).toBeVisible();
	const confirm = dialog.getByRole('button', { name: 'Delete for good' });
	await expect(confirm).toBeDisabled();
	await dialog.getByLabel('To confirm, type notes').fill('notes');
	await confirm.click();
	await expect(dialog).toBeHidden();
	await expect(page.getByRole('status').filter({ hasText: 'Deleted' })).toBeVisible();
	await expect(card).toBeHidden();
	// The focus does not fall to the page's body with the card's button gone.
	await expect(page.locator('body')).not.toBeFocused();

	await expectNoCspViolations(page);
	await page.context().close();
});

test('the pairing dialog on Devices shows a code with its QR and countdown', async ({
	browser,
}) => {
	const page = await openPage(browser, '#devices');

	const open = page.getByRole('button', { name: 'Pair a device' });
	await open.click();
	const dialog = page.getByRole('dialog', { name: 'Pair a device' });
	await expect(dialog.getByRole('heading', { level: 2, name: 'Pair a device' })).toBeFocused();

	const qr = dialog.getByRole('img', { name: 'The pairing code as a QR code' });
	await expect(qr).toBeVisible();
	expect(await qr.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBeGreaterThan(0);
	await expect(dialog.getByText(/Valid for \d+:\d\d/)).toBeVisible();
	await expect(dialog.getByLabel('The code as text')).not.toHaveValue('');

	await dialog.getByRole('button', { name: 'Close' }).click();
	await expect(dialog).toBeHidden();
	await expect(open).toBeFocused();

	await expectNoCspViolations(page);
	await page.context().close();
});

test('the language follows the browser and then the choice, which a reload keeps', async ({
	browser,
}) => {
	for (const [locale, text, lang] of [
		['ru-RU', 'Устройства', 'ru'],
		['pl-PL', 'Urządzenia', 'pl'],
		['de-DE', 'Devices', 'en'],
	]) {
		const page = await openPage(browser, '', { locale });
		await expect(nav(page).getByText(text)).toBeVisible();
		await expect(page.locator('html')).toHaveAttribute('lang', lang);
		await page.context().close();
	}

	const page = await openPage(browser);
	await page.getByRole('button', { name: 'Choose language' }).click();
	await page.getByRole('menu', { name: 'Language' }).getByText('Polski').click();
	await expect(nav(page).getByText('Urządzenia')).toBeVisible();
	await expect(page.locator('html')).toHaveAttribute('lang', 'pl');

	await page.reload();
	await expect(nav(page).getByText('Urządzenia')).toBeVisible();
	await expectNoCspViolations(page);
	await page.context().close();
});

test('the theme follows the system and then the choice, which a reload keeps', async ({
	browser,
}) => {
	const page = await openPage(browser, '', { colorScheme: 'dark' });
	const app = page.locator('.v-application');
	await expect(app).toHaveClass(/v-theme--dark/);

	await page.getByRole('button', { name: 'Choose theme' }).click();
	await page.getByRole('menu', { name: 'Theme' }).getByText('Light').click();
	await expect(app).toHaveClass(/v-theme--light/);

	await page.reload();
	await expect(app).toHaveClass(/v-theme--light/);
	await expectNoCspViolations(page);
	await page.context().close();
});

// Last: it stops the server the other tests use.
test('stopping the server replaces the page with the stopped panel', async ({ browser }) => {
	const page = await openPage(browser);

	await page.getByRole('button', { name: 'Stop the server' }).click();
	await page
		.getByRole('dialog', { name: 'Stop the server?' })
		.getByRole('button', { name: 'Stop', exact: true })
		.click();

	const heading = page.getByRole('heading', { level: 1, name: 'The server has stopped' });
	await expect(heading).toBeVisible();
	await expect(heading).toBeFocused();
	await expect(nav(page)).toBeHidden();
	await page.context().close();
});
