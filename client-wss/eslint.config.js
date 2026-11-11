import js from '@eslint/js';
import globals from 'globals';
import pluginVue from 'eslint-plugin-vue';
import pluginVueA11y from 'eslint-plugin-vuejs-accessibility';
import { defineConfigWithVueTs, vueTsConfigs } from '@vue/eslint-config-typescript';
import skipFormatting from '@vue/eslint-config-prettier/skip-formatting';

export default defineConfigWithVueTs(
	{
		name: 'page/files-to-lint',
		files: ['**/*.{ts,mts,vue}'],
	},
	{
		name: 'page/files-to-ignore',
		ignores: ['coverage/**', 'test-results/**', 'playwright-report/**'],
	},
	{
		name: 'page/browser',
		files: ['src/**/*.{ts,vue}'],
		languageOptions: { globals: { ...globals.browser } },
	},
	{
		name: 'page/node',
		files: ['e2e/**/*.ts', '*.config.{ts,js}'],
		languageOptions: { globals: { ...globals.node } },
	},
	js.configs.recommended,
	pluginVue.configs['flat/recommended'],
	pluginVueA11y.configs['flat/recommended'],
	vueTsConfigs.recommended,
	skipFormatting,
	{
		name: 'page/rules',
		rules: {
			'vue/block-order': ['error', { order: ['style', 'template', 'script'] }],
			'vue/component-name-in-template-casing': [
				'error',
				'PascalCase',
				{ registeredComponentsOnly: false },
			],
			// App names, device names and refused ids come from elsewhere: nothing
			// on the page is ever parsed as HTML.
			'vue/no-v-html': 'error',
			'vuejs-accessibility/no-autofocus': 'off',
		},
	},
);
