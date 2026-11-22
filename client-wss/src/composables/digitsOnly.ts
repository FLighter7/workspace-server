/**
 * Keeps a number field to digits, for a field typed as text so that the
 * browser draws no spin buttons: a typed letter, sign or separator does not
 * land, and a pasted or dropped text keeps only its digits. Bound to the
 * field's `beforeinput`; the rule under the field still checks the range.
 */
export function digitsOnly(event: InputEvent): void {
	const text = event.data ?? event.dataTransfer?.getData('text/plain') ?? null;
	if (text === null || !/\D/.test(text)) return;
	event.preventDefault();
	const digits = text.replace(/\D/g, '');
	const field = event.target;
	if (!digits || !(field instanceof HTMLInputElement)) return;
	field.setRangeText(digits, field.selectionStart ?? 0, field.selectionEnd ?? 0, 'end');
	field.dispatchEvent(new Event('input', { bubbles: true }));
}
