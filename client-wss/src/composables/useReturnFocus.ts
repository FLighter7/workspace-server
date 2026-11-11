import { nextTick, watch, type Ref } from 'vue';

/**
 * Focuses what had the focus when the dialog opened, or, when the dialog took
 * it away (a deleted app's or a removed device's button), the section's
 * heading: never the document body, where a keyboard or screen reader user
 * would have to start the page over.
 */
function restore(anchor: HTMLElement | null): void {
	if (anchor === null) return;
	if (anchor.isConnected) {
		anchor.focus();
		return;
	}
	document.querySelector<HTMLElement>('main h1')?.focus();
}

/**
 * Hands the focus back to whatever had it when the dialog opened. Vuetify does
 * that only for a dialog opened through its activator slot, and every dialog
 * here opens through v-model. The list behind it may change in the meantime:
 * the dialog's action reloads it.
 */
export function useReturnFocus(open: Ref<boolean>): void {
	let anchor: HTMLElement | null = null;

	watch(open, (isOpen) => {
		if (isOpen) {
			const active = document.activeElement;
			anchor = active instanceof HTMLElement && active !== document.body ? active : null;
			return;
		}

		const saved = anchor;
		anchor = null;
		// The list reloads after the dialog's action: wait for it to render.
		void nextTick(() => setTimeout(() => restore(saved)));
	});
}

/**
 * After a list reloads under an action of the user's, moves the focus to the
 * section's heading, or to the given element, if the element that had it left
 * with its row.
 */
export async function rescueFocus(target?: HTMLElement | null): Promise<void> {
	await nextTick();
	if (document.activeElement === null || document.activeElement === document.body) {
		(target ?? document.querySelector<HTMLElement>('main h1'))?.focus();
	}
}
