import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { SUPPORTED_LOCALES } from '..';

// Read as files: an import goes through the vue-i18n plugin, which compiles
// each message instead of leaving its text.
const read = (lang: string): Record<string, string> =>
	JSON.parse(readFileSync(resolve(import.meta.dirname, '../locales', `${lang}.json`), 'utf8'));
const en = read('en');
const catalogs: Record<string, Record<string, string>> = { en, ru: read('ru'), pl: read('pl') };
const placeholders = (text: string): string[] => (text.match(/\{\w+\}/g) ?? []).sort();

describe('the locale catalogs', () => {
	it('are the supported languages, one file each', () => {
		const files = readdirSync(resolve(import.meta.dirname, '../locales'))
			.filter((name) => name.endsWith('.json'))
			.map((name) => name.replace(/\.json$/, ''))
			.sort();
		expect(files).toEqual([...SUPPORTED_LOCALES].sort());
	});

	it.each(['ru', 'pl'])('%s has every English key, filled, with the same placeholders', (lang) => {
		const catalog = catalogs[lang];
		expect(Object.keys(catalog).sort()).toEqual(Object.keys(en).sort());
		for (const [key, english] of Object.entries(en)) {
			expect(catalog[key].trim(), `${lang}: ${key}`).not.toBe('');
			expect(placeholders(catalog[key]), `${lang}: ${key}`).toEqual(placeholders(english));
		}
	});

	it('has no characters vue-i18n would read as syntax', () => {
		// @ links another message and | separates plural forms.
		for (const [lang, catalog] of Object.entries(catalogs)) {
			for (const [key, text] of Object.entries(catalog)) {
				expect(text, `${lang}: ${key}`).not.toMatch(/[@|]/);
			}
		}
	});

	// The message for an expired code names the button that makes a new one.
	it.each(Object.keys(catalogs))('%s: "expired" names the New code button', (lang) => {
		const catalog = catalogs[lang];
		expect(catalog.expired).toContain(catalog.newCode);
	});

	// Used keys are checked by the type checker (DefineLocaleMessage); a key no
	// code names would only rot. Keys are named as quoted strings: t('key'),
	// keypath="key", or a MessageKey in data ({ title: 'stopTitle' }).
	it('has no key the page never names', () => {
		const sources: string[] = [];
		const walk = (dir: string): void => {
			for (const name of readdirSync(dir)) {
				const path = join(dir, name);
				if (statSync(path).isDirectory()) {
					if (name !== '__tests__' && name !== 'locales') walk(path);
				} else if (/\.(ts|vue)$/.test(name)) {
					sources.push(readFileSync(path, 'utf8'));
				}
			}
		};
		walk(resolve(import.meta.dirname, '../..'));
		const code = sources.join('\n');

		const unused = Object.keys(en).filter(
			(key) => !code.includes(`'${key}'`) && !code.includes(`"${key}"`),
		);
		expect(unused).toEqual([]);
	});
});
