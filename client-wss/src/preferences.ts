import { reactive } from 'vue';

export const THEMES = ['system', 'light', 'dark'] as const;
export type ThemePreference = (typeof THEMES)[number];

/** What this browser chose on the page. A null locale: none chosen, follow the browser. */
export interface Preferences {
	theme: ThemePreference;
	locale: string | null;
}

export const PREFERENCES_KEY = 'workspace-server.preferences';

function storage(): Storage | null {
	try {
		return window.localStorage;
	} catch {
		// Blocked site data: the getter itself throws.
		return null;
	}
}

/**
 * Reads the saved choices. Anything missing, unreadable or unknown falls back
 * to the defaults: the system theme and the browser's language. The origin
 * includes the port, so a new HTTP port starts from the defaults too.
 */
export function loadPreferences(): Preferences {
	let saved: Partial<Preferences> = {};
	try {
		const parsed: unknown = JSON.parse(storage()?.getItem(PREFERENCES_KEY) ?? 'null');
		if (parsed !== null && typeof parsed === 'object') saved = parsed as Partial<Preferences>;
	} catch {
		// Corrupt JSON or blocked storage: the defaults.
	}

	return {
		theme: THEMES.includes(saved.theme as ThemePreference)
			? (saved.theme as ThemePreference)
			: 'system',
		locale: typeof saved.locale === 'string' ? saved.locale : null,
	};
}

function save(): void {
	try {
		storage()?.setItem(
			PREFERENCES_KEY,
			JSON.stringify({ theme: preferences.theme, locale: preferences.locale }),
		);
	} catch {
		// Full or blocked storage: the choice still holds until the page reloads.
	}
}

export const preferences = reactive<Preferences>(loadPreferences());

export function setTheme(theme: ThemePreference): void {
	preferences.theme = theme;
	save();
}

export function setLocale(locale: string): void {
	preferences.locale = locale;
	save();
}
