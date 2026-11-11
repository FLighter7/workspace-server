import { serverApi } from '@/api/client';
import type { Status } from '@/api/types';
import type { Message } from './messages';

export const RESTART_POLL_MS = 500;
export const RESTART_WAIT_MS = 30_000;

/**
 * Polls the status until done accepts it, then reloads the page; after
 * RESTART_WAIT_MS it says the server did not come back.
 */
export async function waitRestart(
	say: (message: Message) => void,
	done: (status: Status) => boolean,
): Promise<void> {
	say({ key: 'restarting' });
	const until = Date.now() + RESTART_WAIT_MS;
	while (Date.now() < until) {
		await new Promise((resolve) => setTimeout(resolve, RESTART_POLL_MS));
		try {
			if (done(await serverApi.status())) {
				location.reload();
				return;
			}
		} catch {
			// Stopped for the restart; try again.
		}
	}

	say({ key: 'restartFailed', error: true });
}
