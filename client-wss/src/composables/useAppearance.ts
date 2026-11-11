import { watchEffect } from 'vue';
import { useI18n } from 'vue-i18n';
import { useLocale, useTheme } from 'vuetify';
import { initialLocale, isSupported } from '@/i18n';
import { preferences } from '@/preferences';

/**
 * Keeps Vuetify's theme in step with the chosen one. 'system' is Vuetify's
 * own: it follows the OS's light or dark, live.
 */
export function useAppTheme(): void {
	const theme = useTheme();

	watchEffect(() => {
		theme.change(preferences.theme);
	});

	// The <html> background shows on overscroll and before Vuetify paints. Set
	// through the CSSOM, which the CSP allows, unlike a style attribute.
	watchEffect(() => {
		document.documentElement.style.backgroundColor = theme.current.value.colors
			.background as string;
	});
}

/** Keeps the page's language, Vuetify's and <html lang> on the chosen one. */
export function useAppLocale(): void {
	const { locale } = useI18n();
	const vuetifyLocale = useLocale();

	watchEffect(() => {
		const code = isSupported(preferences.locale) ? preferences.locale : initialLocale;
		locale.value = code;
		vuetifyLocale.current.value = code;
		document.documentElement.lang = code;
	});
}
