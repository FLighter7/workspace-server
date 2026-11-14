import { useI18n } from 'vue-i18n';

const UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte'] as const;

/** One MB as the page counts it, and as the quota field takes it: 1024 × 1024 bytes. */
export const MB = 1024 * 1024;

/** The quota's range in the page's MB, as internal/apps bounds it: 1 MB to 1 TB. */
export const MIN_QUOTA_MB = 1;
export const MAX_QUOTA_MB = 1024 * 1024;

/**
 * A size in binary units, as Windows shows them and as the quota is set
 * (50 MB is 50 × 1024 × 1024 bytes): whole bytes, then at most one decimal
 * of kB, MB, GB, TB.
 */
export function formatBytes(bytes: number, lang: string): string {
	let value = bytes;
	let unit = 0;
	while (value >= 1024 && unit < UNITS.length - 1) {
		value /= 1024;
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
