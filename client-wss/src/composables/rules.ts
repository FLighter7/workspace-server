import { useI18n } from 'vue-i18n';

/** The field rules the forms share, with the page's own messages. */
export function useRules() {
	const { t, n } = useI18n();

	return {
		required: (value: string): true | string => value.trim() !== '' || t('fieldRequired'),
		/**
		 * A whole number from min to max, inclusive. With `named` false the message
		 * names the least alone: the top is a bound nobody should be told of.
		 */
		wholeNumber:
			(min: number, max: number, named = true) =>
			(value: string): true | string => {
				const number = Number(value.trim());
				return (
					(/^\d+$/.test(value.trim()) && number >= min && number <= max) ||
					(named ? t('numberRange', { min: n(min), max: n(max) }) : t('numberMin', { min: n(min) }))
				);
			},
	};
}
