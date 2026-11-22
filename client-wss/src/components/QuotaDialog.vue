<template>
	<VDialog v-model="open" max-width="560" :aria-labelledby="titleId" @after-enter="focusInput">
		<VCard v-if="app" variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">
				<I18nT keypath="quotaTitle" scope="global">
					<template #name>
						<bdi>{{ app.name }}</bdi>
					</template>
				</I18nT>
			</VCardTitle>

			<VForm ref="form" @submit.prevent="save">
				<VCardText class="pt-0">
					<UsageBar
						:size="app.size"
						:quota="app.quota"
						:label="t('quotaUsed', { app: app.name })"
					/>
					<p class="mt-4 mb-6">{{ t('quotaText') }}</p>
					<VTextField
						ref="input"
						v-model="megabytes"
						:label="t('quotaField')"
						inputmode="numeric"
						autocomplete="off"
						:suffix="t('quotaUnit')"
						:hint="bounds"
						persistent-hint
						:rules="[quotaRule]"
						@beforeinput="digitsOnly"
					/>
					<StatusLine :message="message" />
				</VCardText>

				<VCardActions>
					<VSpacer />
					<VBtn variant="text" @click="open = false">{{ t('cancel') }}</VBtn>
					<VBtn type="submit" color="primary" variant="flat" :disabled="busy">
						{{ t('save') }}
					</VBtn>
				</VCardActions>
			</VForm>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, useId, useTemplateRef, watch } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { serverApi } from '@/api/client';
import type { AppInfo } from '@/api/types';
import { digitsOnly } from '@/composables/digitsOnly';
import { errorMessage, type Message } from '@/composables/messages';
import { useRules } from '@/composables/rules';
import { MB, MIN_QUOTA_MB } from '@/composables/useFormat';
import { useQuotaMax } from '@/composables/useQuotaMax';
import { useReturnFocus } from '@/composables/useReturnFocus';
import StatusLine from './StatusLine.vue';
import UsageBar from './UsageBar.vue';

/**
 * The app's quota: the space its files take of it (UsageBar), what the quota
 * is, and the field, in whole MB, from what the files take already (never
 * under the least a quota can be) to what the disk has free; the server
 * refuses a quota under the files too. The field takes digits alone.
 */
const props = defineProps<{ app: AppInfo | null }>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ saved: [quota: number] }>();

const { t, n } = useI18n();
const { maxMb, known, refresh } = useQuotaMax();
const rules = useRules();
/** The least the quota can be: what the files take already, in whole MB. */
const leastMb = computed(() => Math.max(MIN_QUOTA_MB, Math.ceil((props.app?.size ?? 0) / MB)));
/** The most: what the disk has free, though never under the least. */
const mostMb = computed(() => Math.max(leastMb.value, maxMb.value));
/** "From … to … MB", or the least alone while the free space is not known. */
const bounds = computed(() =>
	known.value
		? t('quotaBounds', { min: n(leastMb.value), max: n(mostMb.value) })
		: t('quotaRangeMin', { min: n(leastMb.value) }),
);
/** The quota against the room the disk has, which is read again on each opening. */
const quotaRule = (value: string): true | string =>
	rules.wholeNumber(leastMb.value, mostMb.value, known.value)(value);
const titleId = useId();
const form = useTemplateRef<VForm>('form');
const input = useTemplateRef<{ focus: () => void }>('input');
const megabytes = ref('');
/** What the field showed when the dialog opened: a quota of no whole MB is rounded there. */
const shown = ref('');
const busy = ref(false);
const message = ref<Message | null>(null);

useReturnFocus(open);
onMounted(() => void refresh());

// Filled once on opening: the list behind the dialog is polled, and a new
// copy of the app must not undo what is being typed.
watch(open, (isOpen) => {
	if (!isOpen) return;
	megabytes.value = props.app ? String(Math.max(1, Math.round(props.app.quota / MB))) : '';
	shown.value = megabytes.value;
	busy.value = false;
	message.value = null;
	void refresh(props.app?.size ?? 0);
});

function focusInput(): void {
	input.value?.focus();
}

async function save(): Promise<void> {
	const app = props.app;
	if (!app) return;
	// Saved as shown, a quota of no whole MB (apps.json edited by hand) would
	// be rounded: a field left as it was changes nothing, even when the files
	// have outgrown it since.
	if (megabytes.value.trim() === shown.value) {
		open.value = false;
		return;
	}
	if (!(await form.value?.validate())?.valid) return;
	const quota = Number(megabytes.value) * MB;
	busy.value = true;
	try {
		await serverApi.setQuota(app.id, quota);
	} catch (error) {
		message.value = errorMessage(error);
		busy.value = false;
		return;
	}
	open.value = false;
	emit('saved', quota);
}
</script>
