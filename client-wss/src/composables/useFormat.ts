import { useI18n } from 'vue-i18n';

const UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte'] as const;

/** A size in decimal units: whole bytes, then one decimal of kB, MB, GB. */
export function formatBytes(bytes: number, lang: string): string {
	let value = bytes;
	let unit = 0;
	while (value >= 1000 && unit < UNITS.length - 1) {
		value /= 1000;
		unit++;
	}

	return new Intl.NumberFormat(lang, {
		style: 'unit',
		unit: UNITS[unit],
		unitDisplay: unit === 0 ? 'long' : 'short',
		maximumFractionDigits: unit === 0 ? 0 : 1,
	}).format(value);
}

/** Numbers, sizes and times in the page's current language; read in a template, they follow a switch. */
export function useFormat() {
	const { locale } = useI18n();

	return {
		bytes: (bytes: number): string => formatBytes(bytes, locale.value),
		number: (value: number): string => new Intl.NumberFormat(locale.value).format(value),
		time: (iso: string): string =>
			new Intl.DateTimeFormat(locale.value, { dateStyle: 'medium', timeStyle: 'short' }).format(
				new Date(iso),
			),
	};
}
