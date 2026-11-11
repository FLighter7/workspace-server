import { createI18n } from 'vue-i18n';
import usFlag from 'flag-icons/flags/4x3/us.svg';
import ruFlag from 'flag-icons/flags/4x3/ru.svg';
import plFlag from 'flag-icons/flags/4x3/pl.svg';
import { preferences } from '@/preferences';
import en from './locales/en.json';
import ru from './locales/ru.json';
import pl from './locales/pl.json';

export const SUPPORTED_LOCALES = ['en', 'ru', 'pl'] as const;

export type AppLocale = (typeof SUPPORTED_LOCALES)[number];

export type MessageKey = keyof typeof en;

/** Autonyms for the language picker: each name is written in its own language. */
export const LOCALE_NAMES: Record<AppLocale, string> = {
	en: 'English',
	ru: 'Русский',
	pl: 'Polski',
};

/** English has no single national flag: the US flag stands in, as in the todo app. */
export const LOCALE_FLAGS: Record<AppLocale, string> = {
	en: usFlag,
	ru: ruFlag,
	pl: plFlag,
};

export function isSupported(value: unknown): value is AppLocale {
	return typeof value === 'string' && (SUPPORTED_LOCALES as readonly string[]).includes(value);
}

/**
 * The language chosen on this page, or else the first of the browser's
 * languages the page has (matching on the primary subtag: "ru-RU" is "ru"), or
 * else English.
 */
export function detectLocale(chosen: string | null, languages: readonly string[]): AppLocale {
	if (isSupported(chosen)) return chosen;

	for (const preferred of languages) {
		const primary = preferred.toLowerCase().split('-')[0];
		if (isSupported(primary)) return primary;
	}

	return 'en';
}

/** Resolved once at startup, so Vuetify starts on the same language as the page. */
export const initialLocale = detectLocale(
	preferences.locale,
	navigator.languages?.length ? navigator.languages : [navigator.language],
);

export const i18n = createI18n({
	legacy: false,
	locale: initialLocale,
	fallbackLocale: 'en',
	messages: { en, ru, pl },
});
