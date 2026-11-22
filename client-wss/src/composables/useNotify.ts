import { reactive } from 'vue';
import type { Message } from './messages';

/**
 * The page's one notification centre, which NotificationCenter renders as a
 * snackbar: what an action came to, said once and gone. One is shown at a
 * time; the ones that come meanwhile wait their turn, in order. A state that
 * lasts (the server cannot be reached) stays in its section's status line.
 */
export const noticeState = reactive<{ open: boolean; current: Message | null; queue: Message[] }>({
	open: false,
	current: null,
	queue: [],
});

/** Says what an action came to: green for what was done, red for an error. */
export function notify(message: Message): void {
	if (noticeState.current) {
		noticeState.queue.push(message);
		return;
	}
	noticeState.current = message;
	noticeState.open = true;
}

/** Closes the one on screen, by its timer or its Close button. */
export function dismiss(): void {
	noticeState.open = false;
}

/** The one on screen has gone: the next one, if any, takes its place. */
export function showNext(): void {
	noticeState.current = noticeState.queue.shift() ?? null;
	noticeState.open = noticeState.current !== null;
}

/** Forgets every notice: for tests, which share the module between them. */
export function resetNotices(): void {
	noticeState.open = false;
	noticeState.current = null;
	noticeState.queue = [];
}
