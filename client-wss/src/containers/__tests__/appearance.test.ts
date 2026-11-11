// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { defineComponent, h } from 'vue';
import { useTheme } from 'vuetify';
import { useAppLocale, useAppTheme } from '@/composables/useAppearance';
import { i18n } from '@/i18n';
import { preferences } from '@/preferences';
import { mountPage, settle } from '@/testing/mountPage';
import LocaleMenu from '../LocaleMenu.vue';
import ThemeMenu from '../ThemeMenu.vue';

let themeName = '';

/** The header's two menus, with what App.vue does with their choice. */
const Header = defineComponent({
	setup() {
		useAppTheme();
		useAppLocale();
		const theme = useTheme();
		return () => {
			themeName = theme.name.value;
			return h('div', [h(ThemeMenu), h(LocaleMenu)]);
		};
	},
});

beforeEach(() => {
	localStorage.clear();
	preferences.theme = 'system';
	preferences.locale = 'en';
});

afterEach(() => {
	document.body.innerHTML = '';
});

const buttonLabelled = (label: string): HTMLButtonElement =>
	document.querySelector(`button[aria-label="${label}"]`) as HTMLButtonElement;

const item = (text: string): HTMLElement =>
	[...document.querySelectorAll<HTMLElement>('.v-list-item')].find((el) =>
		el.textContent?.includes(text),
	) as HTMLElement;

describe('the header menus', () => {
	it('switch the language of the page, Vuetify and <html lang>, and keep it', async () => {
		mountPage(Header);
		await settle();

		buttonLabelled('Choose language').click();
		await settle();
		expect(item('English').getAttribute('aria-current')).toBe('true');

		item('Polski').click();
		await settle();
		expect(preferences.locale).toBe('pl');
		expect(i18n.global.locale.value).toBe('pl');
		expect(document.documentElement.lang).toBe('pl');
		expect(JSON.parse(localStorage.getItem('workspace-server.preferences') ?? '{}')).toMatchObject({
			locale: 'pl',
		});
		expect(buttonLabelled('Wybierz język')).not.toBeNull();
	});

	it('switch the theme, and keep it', async () => {
		mountPage(Header);
		await settle();

		buttonLabelled('Choose theme').click();
		await settle();
		expect(item('System').getAttribute('aria-current')).toBe('true');

		item('Dark').click();
		await settle();
		expect(preferences.theme).toBe('dark');
		expect(themeName).toBe('dark');
		expect(JSON.parse(localStorage.getItem('workspace-server.preferences') ?? '{}')).toMatchObject({
			theme: 'dark',
		});
	});
});
