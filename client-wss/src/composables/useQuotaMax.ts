import { ref } from 'vue';
import { serverApi } from '@/api/client';
import { MAX_QUOTA_MB, MB, MIN_QUOTA_MB } from './useFormat';

/**
 * The most a quota field takes: what the data folder's volume still has free,
 * and for an app that already holds files, those too (they are on the volume
 * already). Never past the server's own bound, and never below the least a
 * quota can be. Until the server answers, or when it cannot say, the bound
 * is the server's alone. Read it once the form is mounted, so that the hint
 * does not change under the eye when the form opens, and again on each opening.
 */
export function useQuotaMax() {
	const maxMb = ref(MAX_QUOTA_MB);

	/** Reads the free space again; `held` is the bytes the app's files take now. */
	async function refresh(held = 0): Promise<void> {
		try {
			const { dataFree } = await serverApi.status();
			if (typeof dataFree !== 'number') {
				maxMb.value = MAX_QUOTA_MB;
				return;
			}
			const room = Math.floor((dataFree + held) / MB);
			maxMb.value = Math.min(MAX_QUOTA_MB, Math.max(MIN_QUOTA_MB, room));
		} catch {
			// The server's own bound stays; it checks the quota again.
		}
	}

	return { maxMb, refresh };
}
