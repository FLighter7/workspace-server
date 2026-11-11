import { onBeforeUnmount, onMounted } from 'vue';

export const POLL_MS = 2000;

/**
 * Calls fn now and every intervalMs after the previous call finished, until
 * the returned stop is called. It skips the calls while the page is hidden.
 */
export function startPolling(fn: () => Promise<void>, intervalMs = POLL_MS): () => void {
	let stopped = false;
	let timer: ReturnType<typeof setTimeout> | undefined;

	const tick = async (): Promise<void> => {
		if (stopped) return;
		if (document.visibilityState === 'visible') await fn();
		if (!stopped) timer = setTimeout(tick, intervalMs);
	};
	void tick();

	return () => {
		stopped = true;
		clearTimeout(timer);
	};
}

/** Polls while the component is mounted: a section polls only while it is shown. */
export function usePolling(fn: () => Promise<void>, intervalMs = POLL_MS): void {
	let stop = (): void => {};
	onMounted(() => {
		stop = startPolling(fn, intervalMs);
	});
	onBeforeUnmount(() => stop());
}
