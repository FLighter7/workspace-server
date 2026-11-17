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
					<p class="mb-4">
						{{ t('usedOf', { used: format.bytes(app.size), quota: format.bytes(app.quota) }) }}
					</p>
					<VTextField
						ref="input"
						v-model="megabytes"
						type="number"
						inputmode="numeric"
						:label="t('quotaField')"
						:suffix="t('quotaUnit')"
						:hint="t('quotaHint', { max: n(maxMb) })"
						persistent-hint
						:rules="[quotaRule]"
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
import { onMounted, ref, useId, useTemplateRef, watch } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { serverApi } from '@/api/client';
import type { AppInfo } from '@/api/types';
import { errorMessage, type Message } from '@/composables/messages';
import { useRules } from '@/composables/rules';
import { MB, MIN_QUOTA_MB, useFormat } from '@/composables/useFormat';
import { useQuotaMax } from '@/composables/useQuotaMax';
import { useReturnFocus } from '@/composables/useReturnFocus';
import StatusLine from './StatusLine.vue';

const props = defineProps<{ app: AppInfo | null }>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ saved: [quota: number] }>();

const { t, n } = useI18n();
const format = useFormat();
const { maxMb, refresh } = useQuotaMax();
const rules = useRules();
/** The quota against the room the disk has, which is read again on each opening. */
const quotaRule = (value: string): true | string =>
	rules.wholeNumber(MIN_QUOTA_MB, maxMb.value)(value);
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
	if (!app || !(await form.value?.validate())?.valid) return;
	// Saved as shown, a quota of no whole MB (apps.json edited by hand) would
	// be rounded: a field left as it was changes nothing.
	if (megabytes.value.trim() === shown.value) {
		open.value = false;
		return;
	}
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
