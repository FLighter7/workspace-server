import { describe, expect, it } from 'vitest';
import { detectLocale } from '..';

describe('detectLocale', () => {
	it('takes the language chosen on the page first', () => {
		expect(detectLocale('pl', ['ru-RU', 'en'])).toBe('pl');
	});

	it('ignores a chosen language the page does not have', () => {
		expect(detectLocale('de', ['ru-RU'])).toBe('ru');
	});

	it.each([
		[['ru-RU', 'ru'], 'ru'],
		[['PL-pl'], 'pl'],
		[['de-DE', 'pl'], 'pl'],
		[['uk-UA', 'ru', 'en'], 'ru'],
		[['en-GB'], 'en'],
	])('follows the browser: %j → %s', (languages, want) => {
		expect(detectLocale(null, languages)).toBe(want);
	});

	it('falls back to English', () => {
		expect(detectLocale(null, ['de', 'fr'])).toBe('en');
		expect(detectLocale(null, [])).toBe('en');
	});
});
