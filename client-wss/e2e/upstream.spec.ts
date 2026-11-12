import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { chromium, expect, test, type Browser, type Page } from '@playwright/test';
import { expectNoCspViolations, nav, openPage, recordCspViolations } from './harness';
import { buildServer, decodeCode, onLoopback, SCRATCH, Server } from './servers';

/**
 * Two real servers on this machine: a main ("Desk") and a server linked to it
 * from its page ("Laptop"), as a laptop's server is linked to the PC's. The
 * main holds a document of todo before the link.
 */

let main: Server | null = null;
let sub: Server | null = null;
let tmp = '';

test.describe.configure({ mode: 'serial' });

interface Status {
	serverId: string;
	mdnsName: string;
}

test.beforeAll(async () => {
	// Building the server with Go takes a while on a cold cache.
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'page-e2e-'));
	const binary = buildServer(tmp);
	mkdirSync(join(tmp, 'main', 'apps', 'todo'), { recursive: true });
	writeFileSync(join(tmp, 'main', 'apps', 'todo', 'data.json'), '{"v":1}');
	main = await Server.start(binary, join(tmp, 'main'));
	sub = await Server.start(binary, join(tmp, 'sub'));
	await main.api('PUT', '/api/config', { name: 'Desk' });
	await sub.api('PUT', '/api/config', { name: 'Laptop' });
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

const upstream = (page: Page) => page.getByRole('region', { name: 'Upstream server' });

/** The section's own status line: the last, after the camera's while that is on. */
const said = (page: Page) => upstream(page).getByRole('status').last();

/** The value of the fact labelled `label` in the Upstream server section. */
const fact = (page: Page, label: string) =>
	upstream(page).locator(`div:has(> dt:text-is("${label}")) > dd`);

test('a server links to its main by a pasted code, and shows the link', async ({ browser }) => {
	const { main, sub } = servers();
	const mainStatus = await main.api<Status>('GET', '/api/status');
	const page = await openPage(browser, sub.base, '#server');
	const section = upstream(page);
	await expect(section.getByText('This server is a main: devices pair with it.')).toBeVisible();

	// The main's code with 127.0.0.1 as its host, so that the first link
	// does not depend on the LAN; the main's welcome brings its own hosts.
	const code = onLoopback((await main.pairing()).code);
	await section.getByLabel("The main server's pairing code").fill(code);
	await section.getByRole('button', { name: 'Link', exact: true }).click();
	await expect(said(page)).toHaveText('Linked to Desk.', { timeout: 30_000 });
	await expect(section.getByRole('heading', { level: 2, name: 'Upstream server' })).toBeFocused();

	await expect(fact(page, 'Main server')).toHaveText(`Desk ${mainStatus.serverId}`);
	await expect(fact(page, 'State')).toHaveText('Connected');
	await expect(fact(page, 'Apps synced')).toHaveText('todo');
	await expect(fact(page, 'Last sync')).not.toHaveText('Not yet');
	await expect(section.getByRole('button', { name: "Pair this computer's browser" })).toBeVisible();
	expect(await sub.api('GET', '/api/apps/todo/document')).toEqual({ v: 1 });
	await expect(sub.api('POST', '/api/pairing')).rejects.toThrow(/409.*notMain/);

	await expectNoCspViolations(page);
	await page.context().close();
});

test("the main's Devices shows the linked server as a server, and the main offers no link", async ({
	browser,
}) => {
	const { main, sub } = servers();
	const subStatus = await sub.api<Status>('GET', '/api/status');
	const page = await openPage(browser, main.base, '#devices');

	await expect(page.getByRole('heading', { level: 2, name: 'Linked servers' })).toBeVisible();
	const card = page.getByRole('listitem').filter({
		has: page.getByRole('heading', { level: 3, name: 'Laptop' }),
	});
	await expect(card).toContainText(subStatus.serverId);
	await expect(card).toContainText(subStatus.mdnsName);
	await expect(card).toContainText('Connected now');

	await card.getByRole('button', { name: 'Remove' }).click();
	const dialog = page.getByRole('dialog', { name: 'Remove Laptop?' });
	await expect(dialog).toContainText(
		'The server on that computer stops syncing with this one, and devices no longer sync through it.',
	);
	await dialog.getByRole('button', { name: 'Cancel' }).click();
	await expect(dialog).toBeHidden();

	await nav(page).getByRole('link', { name: 'Server' }).click();
	await expect(upstream(page)).toContainText('Other servers are linked to this one');
	await expect(upstream(page).getByRole('button', { name: 'Link', exact: true })).toHaveCount(0);

	await expectNoCspViolations(page);
	await page.context().close();
});

test("the linked server pairs this computer's browser through the main", async ({ browser }) => {
	const { main, sub } = servers();
	const mainStatus = await main.api<Status>('GET', '/api/status');
	const page = await openPage(browser, sub.base, '#devices');

	await expect(page.getByText('This server is linked to the main server Desk')).toBeVisible();
	await expect(page.getByRole('button', { name: 'Pair a device' })).toHaveCount(0);
	const open = page.getByRole('button', { name: "Pair this computer's browser" });
	await open.click();
	const dialog = page.getByRole('dialog', { name: "Pair this computer's browser" });
	await expect(
		dialog.getByRole('heading', { level: 2, name: "Pair this computer's browser" }),
	).toBeFocused();
	await expect(dialog).toContainText('This is a pairing code of the main server, Desk.');

	const qr = dialog.getByRole('img', { name: 'The pairing code as a QR code' });
	await expect(qr).toBeVisible();
	expect(await qr.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBeGreaterThan(0);
	await expect(dialog.getByText(/Valid for \d+:\d\d/)).toBeVisible();
	const code = await dialog.getByLabel('The code as text').inputValue();
	expect(decodeCode(code).id).toBe(mainStatus.serverId);

	await dialog.getByRole('button', { name: 'Close' }).click();
	await expect(dialog).toBeHidden();
	await expect(open).toBeFocused();
	await expectNoCspViolations(page);
	await page.context().close();
});

test('unlinking asks first, says the data stays, and makes the server a main again', async ({
	browser,
}) => {
	const { main, sub } = servers();
	const page = await openPage(browser, sub.base, '#server');
	const section = upstream(page);

	await section.getByRole('button', { name: 'Unlink' }).click();
	const dialog = page.getByRole('dialog', { name: 'Unlink from Desk?' });
	await expect(dialog).toContainText('No data is deleted');
	await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused();
	await dialog.getByRole('button', { name: 'Unlink' }).click();

	await expect(said(page)).toHaveText('Unlinked. This server is a main again.');
	await expect(section.getByLabel("The main server's pairing code")).toBeVisible();
	expect(await sub.api('GET', '/api/apps/todo/document')).toEqual({ v: 1 });
	const devices = await main.api<{ kind?: string }[]>('GET', '/api/devices');
	expect(devices.filter((device) => device.kind === 'server')).toEqual([]);

	await expectNoCspViolations(page);
	await page.context().close();
});

/**
 * A video of the QR code qr (a data: URL) for Chromium's fake camera: Y4M,
 * grey, the code in the middle of a white frame.
 */
async function qrVideo(browser: Browser, qr: string, file: string): Promise<void> {
	const [width, height, side] = [640, 480, 300];
	const page = await browser.newPage();
	const luma = await page.evaluate(
		async ({ qr, width, height, side }) => {
			const image = new Image();
			image.src = qr;
			await image.decode();
			const canvas = document.createElement('canvas');
			canvas.width = width;
			canvas.height = height;
			const context = canvas.getContext('2d') as CanvasRenderingContext2D;
			context.fillStyle = '#ffffff';
			context.fillRect(0, 0, width, height);
			context.imageSmoothingEnabled = false;
			context.drawImage(image, (width - side) / 2, (height - side) / 2, side, side);
			const { data } = context.getImageData(0, 0, width, height);
			const out: number[] = [];
			for (let i = 0; i < data.length; i += 4) out.push(data[i]);
			return out;
		},
		{ qr, width, height, side },
	);
	await page.close();

	const frame = Buffer.concat([
		Buffer.from('FRAME\n'),
		Buffer.from(luma),
		Buffer.alloc((width / 2) * (height / 2) * 2, 128),
	]);
	writeFileSync(
		file,
		Buffer.concat([Buffer.from(`YUV4MPEG2 W${width} H${height} F10:1 Ip A1:1\n`), frame, frame]),
	);
}

test("a server links by the main's QR code, read by the camera; the main's removal stops the link", async ({
	browser,
}) => {
	test.setTimeout(90_000);
	const { main, sub } = servers();
	const video = join(tmp, 'qr.y4m');
	await qrVideo(browser, (await main.pairing()).qr, video);

	const camera = await chromium.launch({
		args: [
			'--use-fake-ui-for-media-stream',
			'--use-fake-device-for-media-stream',
			`--use-file-for-fake-video-capture=${video}`,
		],
	});
	try {
		const context = await camera.newContext({ locale: 'en-US', permissions: ['camera'] });
		const page = await context.newPage();
		await recordCspViolations(page);
		const requests: string[] = [];
		page.on('request', (request) => requests.push(new URL(request.url()).pathname));
		await page.goto(`${sub.base}/#server`);
		const section = upstream(page);

		await section.getByRole('button', { name: 'Scan with the camera' }).click();
		await expect(said(page)).toHaveText('Linked to Desk.', { timeout: 45_000 });
		await expect(fact(page, 'State')).toHaveText('Connected');
		await expectNoCspViolations(page);
		// Without a native detector, qr-scanner decodes in its worker: a file
		// of the page's own, which the CSP lets in, never a blob.
		if (!(await page.evaluate(() => 'BarcodeDetector' in window))) {
			expect(
				requests.filter((path) => /^\/assets\/qr-scanner-worker-\w+\.js$/.test(path)),
			).toHaveLength(1);
		}

		// The main removes it on its own page.
		const mainPage = await openPage(browser, main.base, '#devices');
		const card = mainPage.getByRole('listitem').filter({
			has: mainPage.getByRole('heading', { level: 3, name: 'Laptop' }),
		});
		await card.getByRole('button', { name: 'Remove' }).click();
		await mainPage
			.getByRole('dialog', { name: 'Remove Laptop?' })
			.getByRole('button', { name: 'Remove' })
			.click();
		await expect(mainPage.getByRole('heading', { level: 2, name: 'Linked servers' })).toHaveCount(
			0,
		);
		await mainPage.context().close();

		await expect(fact(page, 'State')).toHaveText('Stopped', { timeout: 20_000 });
		await expect(section).toContainText('The main removed this server.');
		await expectNoCspViolations(page);
	} finally {
		await camera.close();
	}
});
