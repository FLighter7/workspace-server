// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { loadPreferences, preferences, PREFERENCES_KEY, setLocale, setTheme } from '@/preferences';

beforeEach(() => {
	localStorage.clear();
	preferences.theme = 'system';
	preferences.locale = null;
});

afterEach(() => {
	vi.restoreAllMocks();
});

describe('preferences', () => {
	it('start from the system theme and the browser language', () => {
		expect(loadPreferences()).toEqual({ theme: 'system', locale: null });
	});

	it('keep a choice across a reload', () => {
		setTheme('dark');
		setLocale('pl');
		expect(JSON.parse(localStorage.getItem(PREFERENCES_KEY) ?? '')).toEqual({
			theme: 'dark',
			locale: 'pl',
		});
		expect(loadPreferences()).toEqual({ theme: 'dark', locale: 'pl' });
	});

	it('fall back to the defaults on corrupt or unknown values', () => {
		localStorage.setItem(PREFERENCES_KEY, '{not json');
		expect(loadPreferences()).toEqual({ theme: 'system', locale: null });

		localStorage.setItem(PREFERENCES_KEY, JSON.stringify({ theme: 'sepia', locale: 7 }));
		expect(loadPreferences()).toEqual({ theme: 'system', locale: null });
	});

	it('still hold a choice when storage refuses it', () => {
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
			throw new DOMException('full', 'QuotaExceededError');
		});
		setTheme('light');
		expect(preferences.theme).toBe('light');
	});
});
