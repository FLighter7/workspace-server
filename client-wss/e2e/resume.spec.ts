import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { expectNoCspViolations, openPage } from './harness';
import { buildDevice, buildServer, onLoopback, runDevice, SCRATCH, Server } from './servers';

/**
 * Resuming an app the write guard stopped, on two real servers on this
 * machine: a main ("Desk") and a server linked to it ("Laptop"). A device
 * (`e2e/device`, built with Go here) pairs with the main and writes todo on
 * the laptop's server more often than its link may push it on, so the guard
 * stops todo there. The laptop's page resumes it with one button.
 */

let main: Server | null = null;
let sub: Server | null = null;
let device = '';
let tmp = '';

test.describe.configure({ mode: 'serial' });

interface Upstream {
	connected: boolean;
	apps: string[];
	stopped?: string[];
}

test.beforeAll(async () => {
	test.setTimeout(180_000);
	mkdirSync(SCRATCH, { recursive: true });
	tmp = mkdtempSync(join(SCRATCH, 'page-e2e-resume-'));
	const binary = buildServer(tmp);
	device = buildDevice(tmp);
	mkdirSync(join(tmp, 'main', 'apps', 'todo'), { recursive: true });
	writeFileSync(join(tmp, 'main', 'apps', 'todo', 'data.json'), '{"v":1}');
	main = await Server.start(binary, join(tmp, 'main'));
	sub = await Server.start(binary, join(tmp, 'sub'));
	await main.api('PUT', '/api/config', { name: 'Desk' });
	await sub.api('PUT', '/api/config', { name: 'Laptop' });
	await sub.api('POST', '/api/upstream', { code: onLoopback((await main.pairing()).code) });
	await expect
		.poll(async () => (await sub?.api<Upstream>('GET', '/api/upstream'))?.connected, {
			timeout: 20_000,
		})
		.toBe(true);
	await expect
		.poll(async () => sub?.api('GET', '/api/apps/todo/document').catch(() => null), {
			timeout: 20_000,
		})
		.toEqual({ v: 1 });
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
const said = (page: Page) => upstream(page).getByRole('status').last();
const fact = (page: Page, label: string) =>
	upstream(page).locator(`div:has(> dt:text-is("${label}")) > dd`);

/** Has the device write todo on the laptop until the write guard stops it there. */
async function tripGuard(pushed: number): Promise<string> {
	const { main, sub } = servers();
	const { last } = await runDevice(device, onLoopback((await main.pairing()).code), {
		pushed,
		extra: 1,
	});
	await expect
		.poll(async () => (await sub.api<Upstream>('GET', '/api/upstream')).stopped, {
			timeout: 20_000,
		})
		.toEqual(['todo']);

	return last;
}

test('an app the write guard stopped resumes with one button, at once, and stops again on a loop', async ({
	browser,
}) => {
	test.setTimeout(120_000);
	const { main, sub } = servers();
	const heldBack = await tripGuard(10);
	expect(await sub.api('GET', '/api/apps/todo/document')).toEqual(JSON.parse(heldBack));
	expect(await main.api('GET', '/api/apps/todo/document')).not.toEqual(JSON.parse(heldBack));

	const page = await openPage(browser, sub.base, '#server');
	const section = upstream(page);
	const resume = section.getByRole('button', { name: 'Resume todo' });
	await expect(resume).toBeVisible();
	await expect(fact(page, 'Stopped apps')).toContainText('todo');
	await expect(section).toContainText(
		'“Resume” syncs an app with the main again at once; a restart of this server resumes them all.',
	);

	// No question: the app syncs again at once, pushing what the guard held
	// back, and the list follows.
	await resume.click();
	await expect(page.getByRole('dialog')).toHaveCount(0);
	await expect(said(page)).toHaveText('todo syncs with the main again.');
	await expect(section.getByRole('heading', { level: 2, name: 'Upstream server' })).toBeFocused();
	await expect(fact(page, 'Stopped apps')).toHaveCount(0);
	await expect(fact(page, 'Apps synced')).toHaveText('todo');
	await expect
		.poll(async () => main.api('GET', '/api/apps/todo/document'), { timeout: 20_000 })
		.toEqual(JSON.parse(heldBack));

	// The guard stays: with the push at the resume, nine more fill a fresh
	// minute, and the next stops todo again; the page offers Resume again.
	await tripGuard(9);
	await expect(resume).toBeVisible({ timeout: 10_000 });
	await expect(fact(page, 'Apps synced')).toHaveText('none');

	await expectNoCspViolations(page);
	await page.context().close();
});
