import { expect, type Browser, type BrowserContextOptions, type Page } from '@playwright/test';

/** What the e2e pages share: a page that records every CSP violation it meets. */

declare global {
	interface Window {
		cspViolations: string[];
	}
}

/** Records the CSP violations of every document the page loads from now on. */
export async function recordCspViolations(page: Page): Promise<void> {
	await page.addInitScript(() => {
		window.cspViolations = [];
		document.addEventListener('securitypolicyviolation', (event) => {
			window.cspViolations.push(`${event.violatedDirective}: ${event.blockedURI}`);
		});
	});
}

/** A page of the server at base in a fresh context, in English, that records every CSP violation. */
export async function openPage(
	browser: Browser,
	base: string,
	hash = '',
	options: BrowserContextOptions = {},
): Promise<Page> {
	const context = await browser.newContext({ locale: 'en-US', ...options });
	const page = await context.newPage();
	await recordCspViolations(page);
	await page.goto(`${base}/${hash}`);
	await expect(page.getByRole('heading', { level: 1 })).toBeVisible();

	return page;
}

export async function expectNoCspViolations(page: Page): Promise<void> {
	expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
}

// By role alone: its name is translated too.
export const nav = (page: Page) => page.getByRole('navigation');
