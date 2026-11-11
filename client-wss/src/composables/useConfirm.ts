import { reactive } from 'vue';
import type { MessageKey } from '@/i18n';

export interface Question {
	title: MessageKey;
	/** The {name} in the title, shown isolated. */
	name?: string;
	text: MessageKey;
	ok: MessageKey;
}

/** The one confirmation dialog's state; ConfirmDialog renders it. */
export const confirmState = reactive<{ open: boolean; question: Question | null }>({
	open: false,
	question: null,
});

let answer: ((confirmed: boolean) => void) | null = null;

/** Shows a modal question and resolves to true when the user confirms. */
export function ask(question: Question): Promise<boolean> {
	answer?.(false);
	confirmState.question = question;
	confirmState.open = true;

	return new Promise((resolve) => {
		answer = resolve;
	});
}

/** Closes the question with the user's answer; closing it any other way is a no. */
export function settle(confirmed: boolean): void {
	confirmState.open = false;
	answer?.(confirmed);
	answer = null;
}
