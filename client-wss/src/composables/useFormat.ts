import { useI18n } from 'vue-i18n';

const UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte'] as const;

/** One MB as the page counts it, and as the quota field takes it: 1024 × 1024 bytes. */
export const MB = 1024 * 1024;

/** The least quota the page offers, in MB; the server takes down to 1 MB. */
export const MIN_QUOTA_MB = 10;
/** The most the page can count exactly, in MB: 2^53 bytes, as far as the server bounds it. */
export const MAX_QUOTA_MB = Math.floor(Number.MAX_SAFE_INTEGER / MB);

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

/** A moment in short: the day, the month and the time, and the year only when it is not `now`'s. */
export function formatWhen(iso: string, lang: string, now = new Date()): string {
	const date = new Date(iso);
	const year = date.getFullYear() === now.getFullYear() ? {} : { year: 'numeric' as const };

	return new Intl.DateTimeFormat(lang, {
		day: 'numeric',
		month: 'short',
		...year,
		hour: 'numeric',
		minute: '2-digit',
	}).format(date);
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
		/** A moment in short: the day, the month and the time; the year only when it is not this one. */
		when: (iso: string): string => formatWhen(iso, locale.value),
	};
}
