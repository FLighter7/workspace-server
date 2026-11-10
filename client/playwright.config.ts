import { defineConfig, devices } from '@playwright/test';

const PORT = 4180;

export default defineConfig({
	testDir: './e2e',
	retries: process.env.CI ? 1 : 0,
	reporter: 'list',
	use: {
		baseURL: `http://localhost:${PORT}/`,
		trace: 'on-first-retry',
	},
	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
	webServer: {
		command: `node e2e/serve.mjs ${PORT}`,
		url: `http://localhost:${PORT}/e2e/page/index.html`,
		reuseExistingServer: !process.env.CI,
		timeout: 30_000,
	},
});
