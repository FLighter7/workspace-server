import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createSocket } from 'node:dgram';
import { existsSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { join, resolve } from 'node:path';
import {
	expect,
	test,
	type Browser,
	type BrowserContextOptions,
	type Page,
} from '@playwright/test';

/**
 * The built page in a real workspace-server, in a real browser. The server is
 * built with Go from this repository, embedding internal/web/ui/dist (which
 * `pnpm run test:e2e` builds first), or WORKSPACE_SERVER_BIN runs one already
 * built. Its data stays under the repository's tmp/.
 */

declare global {
	interface Window {
		cspViolations: string[];
	}
}

const REPO = resolve(import.meta.dirname, '..', '..');

let server: ChildProcess | null = null;
let tmp = '';
let base = '';

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
	if (!existsSync(join(REPO, 'internal', 'web', 'ui', 'dist', 'index.html'))) {
		throw new Error('the page is not built: run `pnpm run build` first');
	}

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
			const response = await fetch(`${base}/api/status`);
			if (response.ok) return;
		} catch {
			// Not listening yet.
		}
		await new Promise((done) => setTimeout(done, 200));
	}

	throw new Error('workspace-server did not start within 30 s');
}

test.beforeAll(async () => {
	// Building the server with Go takes a while on a cold cache.
	test.setTimeout(180_000);
	const scratch = join(REPO, 'tmp');
	mkdirSync(scratch, { recursive: true });
	tmp = mkdtempSync(join(scratch, 'page-e2e-'));
	const binary = buildServer();
	const httpPort = await freeTcpPort();
	const udpPort = await freeUdpPort();
	// localhost, as the program opens it: the browser tries ::1 first and falls back.
	base = `http://localhost:${httpPort}`;

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

/** A page in a fresh context that records every CSP violation it meets. */
async function openPage(
	browser: Browser,
	hash = '',
	options: BrowserContextOptions = {},
): Promise<Page> {
	const context = await browser.newContext({ locale: 'en-US', ...options });
	await context.addInitScript(() => {
		window.cspViolations = [];
		document.addEventListener('securitypolicyviolation', (event) => {
			window.cspViolations.push(`${event.violatedDirective}: ${event.blockedURI}`);
		});
	});
	const page = await context.newPage();
	await page.goto(`${base}/${hash}`);
	await expect(page.getByRole('heading', { level: 1 })).toBeVisible();

	return page;
}

async function expectNoCspViolations(page: Page): Promise<void> {
	expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
}

// By role alone: its name is translated too.
const nav = (page: Page) => page.getByRole('navigation');

test('every section opens from the header, and the CSP blocks nothing', async ({ browser }) => {
	const page = await openPage(browser);

	await expect(page.getByRole('heading', { level: 1, name: 'Server' })).toBeVisible();
	await expect(page.getByText('Server ID')).toBeVisible();
	await expect(page.getByLabel('UDP port')).not.toHaveValue('');

	for (const [link, heading] of [
		['Apps', 'Apps'],
		['Devices', 'Devices'],
		['Pair a device', 'Pair a device'],
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
	await expect(page.getByRole('heading', { level: 2, name: 'Todo todo' })).toBeVisible();
	await page.getByLabel('ID', { exact: true }).fill('todo');
	await expect(page.getByText('An app with this ID already exists.')).toBeVisible();
	await page.getByLabel('ID', { exact: true }).fill('Bad ID');
	await expect(
		page.getByText('Use only lowercase Latin letters, digits and hyphens, 1 to 64 of them.'),
	).toBeVisible();
	await page.getByLabel('ID', { exact: true }).fill('notes');
	await page.getByLabel('Name', { exact: true }).fill('My notes');
	await page.getByRole('button', { name: 'Add', exact: true }).click();
	await expect(page.getByRole('status').filter({ hasText: 'Added.' })).toBeVisible();
	const card = page.getByRole('listitem').filter({
		has: page.getByRole('heading', { name: 'My notes notes' }),
	});
	await expect(card).toBeVisible();

	await card.getByRole('button', { name: 'Delete', exact: true }).click();
	const dialog = page.getByRole('dialog', { name: 'Delete My notes?' });
	await expect(dialog).toBeVisible();
	const confirm = dialog.getByRole('button', { name: 'Delete for good' });
	await expect(confirm).toBeDisabled();
	await dialog.getByLabel('To confirm, type notes').fill('notes');
	await confirm.click();
	await expect(dialog).toBeHidden();
	await expect(page.getByRole('status').filter({ hasText: 'Deleted.' })).toBeVisible();
	await expect(card).toBeHidden();
	// The focus does not fall to the page's body with the card's button gone.
	await expect(page.locator('body')).not.toBeFocused();

	await expectNoCspViolations(page);
	await page.context().close();
});

test('the Pair section shows a code with its QR and countdown', async ({ browser }) => {
	const page = await openPage(browser, '#pair');

	const qr = page.getByRole('img', { name: 'The pairing code as a QR code' });
	await expect(qr).toBeVisible();
	expect(await qr.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBeGreaterThan(0);
	await expect(page.getByText(/Valid for \d+:\d\d/)).toBeVisible();
	await expect(page.getByLabel('The code as text')).not.toHaveValue('');

	await expectNoCspViolations(page);
	await page.context().close();
});

test('the language follows the browser and then the choice, which a reload keeps', async ({
	browser,
}) => {
	for (const [locale, text, lang] of [
		['ru-RU', 'Подключить устройство', 'ru'],
		['pl-PL', 'Sparuj urządzenie', 'pl'],
		['de-DE', 'Pair a device', 'en'],
	]) {
		const page = await openPage(browser, '', { locale });
		await expect(nav(page).getByText(text)).toBeVisible();
		await expect(page.locator('html')).toHaveAttribute('lang', lang);
		await page.context().close();
	}

	const page = await openPage(browser);
	await page.getByRole('button', { name: 'Choose language' }).click();
	await page.getByRole('dialog', { name: 'Language' }).getByText('Polski').click();
	await expect(nav(page).getByText('Sparuj urządzenie')).toBeVisible();
	await expect(page.locator('html')).toHaveAttribute('lang', 'pl');

	await page.reload();
	await expect(nav(page).getByText('Sparuj urządzenie')).toBeVisible();
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
	await page.getByRole('dialog', { name: 'Theme' }).getByText('Light').click();
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
