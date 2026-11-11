import { defineConfig, devices } from '@playwright/test';

// e2e/page.spec.ts builds and starts a real workspace-server itself, with the
// page built into it; there is no web server to start here.
export default defineConfig({
	testDir: './e2e',
	retries: process.env.CI ? 1 : 0,
	reporter: 'list',
	use: { trace: 'on-first-retry' },
	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
