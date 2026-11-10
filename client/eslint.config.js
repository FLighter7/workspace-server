import js from '@eslint/js';
import { defineConfig } from 'eslint/config';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default defineConfig(
	{
		name: 'client/ignores',
		ignores: ['dist/**', 'coverage/**', 'test-results/**', 'playwright-report/**'],
	},
	js.configs.recommended,
	tseslint.configs.recommended,
	{
		name: 'client/browser',
		files: ['src/**/*.ts', 'e2e/page/**/*.js'],
		languageOptions: { globals: { ...globals.browser } },
	},
	{
		name: 'client/node',
		files: ['test/**/*.ts', 'e2e/*.{ts,mjs}', '*.config.{ts,js}'],
		languageOptions: { globals: { ...globals.node } },
	},
);
