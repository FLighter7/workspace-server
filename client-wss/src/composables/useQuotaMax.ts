import { computed, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { serverApi } from '@/api/client';
import { MAX_QUOTA_MB, MB, MIN_QUOTA_MB } from './useFormat';

/**
 * The most a quota field takes: what the data folder's volume still has free,
 * and for an app that already holds files, those too (they are on the volume
 * already). It is never below the least a quota can be. When the server cannot
 * say what is free, or has not answered yet, only the exact top of a number
 * bounds it, which no disk reaches, and the hint names the least alone. Read
 * it once the form is mounted, so that the hint does not change under the eye
 * when the form opens, and again on each opening.
 */
export function useQuotaMax() {
	const { t, n } = useI18n();
	const maxMb = ref(MAX_QUOTA_MB);
	const known = ref(false);

	/** The range as a sentence of the hint, to the free space when it is known. */
	const range = computed(() =>
		known.value
			? t('quotaRange', { min: n(MIN_QUOTA_MB), max: n(maxMb.value) })
			: t('quotaRangeMin', { min: n(MIN_QUOTA_MB) }),
	);

	/** Reads the free space again; `held` is the bytes the app's files take now. */
	async function refresh(held = 0): Promise<void> {
		try {
			const { dataFree } = await serverApi.status();
			if (typeof dataFree !== 'number') {
				known.value = false;
				maxMb.value = MAX_QUOTA_MB;
				return;
			}
			const room = Math.floor((dataFree + held) / MB);
			maxMb.value = Math.min(MAX_QUOTA_MB, Math.max(MIN_QUOTA_MB, room));
			known.value = true;
		} catch {
			// What was known stays; the server checks the quota again.
		}
	}

	return { maxMb, known, range, refresh };
}
