<style lang="scss" module>
.fields {
	display: grid;
	gap: 1rem;
}
</style>

<template>
	<VDialog v-model="open" max-width="560" :aria-labelledby="titleId" @after-enter="focusInput">
		<VCard variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">{{ t('addApp') }}</VCardTitle>

			<VForm ref="form" @submit.prevent="add">
				<VCardText :class="['pt-0', $style.fields]">
					<VTextField
						ref="input"
						v-model="id"
						:label="t('appId')"
						:hint="t('appIdHint')"
						persistent-hint
						maxlength="64"
						:rules="[idRule]"
						autocomplete="off"
						autocapitalize="none"
						spellcheck="false"
					/>
					<VTextField
						v-model="name"
						:label="t('appName')"
						:hint="t('appNameHint')"
						persistent-hint
						autocomplete="off"
					/>
					<VTextField
						v-model="quota"
						type="number"
						inputmode="numeric"
						:label="t('quotaField')"
						:hint="t(linked ? 'quotaAddLinkedHint' : 'quotaAddHint')"
						persistent-hint
						:rules="[quotaRule]"
					/>
					<StatusLine :message="message" />
				</VCardText>

				<VCardActions>
					<VSpacer />
					<VBtn variant="text" @click="open = false">{{ t('cancel') }}</VBtn>
					<VBtn type="submit" color="primary" variant="flat" :disabled="busy">{{ t('add') }}</VBtn>
				</VCardActions>
			</VForm>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { nextTick, ref, useId, useTemplateRef, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { serverApi } from '@/api/client';
import { ID_PATTERN, RESERVED_ID } from '@/api/ids';
import type { NewApp } from '@/api/types';
import { boot } from '@/boot';
import { errorMessage, type Message } from '@/composables/messages';
import { useRules } from '@/composables/rules';
import { MAX_QUOTA_MB, MB, MIN_QUOTA_MB } from '@/composables/useFormat';
import { useReturnFocus } from '@/composables/useReturnFocus';
import StatusLine from './StatusLine.vue';

/**
 * The form for a new app: its id, its name, its quota. `ids` are the apps the
 * server serves already, and `linked` says that this server is a subordinate,
 * where the main's quota replaces this one once the main lists the app too.
 * The server checks everything again; its refusal stays in the dialog.
 */
const props = defineProps<{ ids: string[]; linked: boolean }>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ added: [] }>();

const { t } = useI18n();
const quotaRule = useRules().wholeNumber(MIN_QUOTA_MB, MAX_QUOTA_MB);
const defaultQuota = String(Math.round(boot.quota / MB));
const titleId = useId();
const form = useTemplateRef<VForm>('form');
const input = useTemplateRef<{ focus: () => void }>('input');
const id = ref('');
const name = ref('');
const quota = ref(defaultQuota);
const busy = ref(false);
const message = ref<Message | null>(null);

useReturnFocus(open);

// An empty form on each opening: what was typed and cancelled is not kept.
watch(open, async (isOpen) => {
	if (!isOpen) return;
	id.value = '';
	name.value = '';
	quota.value = defaultQuota;
	busy.value = false;
	message.value = null;
	await nextTick();
	form.value?.resetValidation();
});

function focusInput(): void {
	input.value?.focus();
}

/** The id being typed, against the pattern and the ids in use; the server checks again. */
function idRule(value: string): true | string {
	if (value === '') return t('fieldRequired');
	if (!ID_PATTERN.test(value)) return t('idInvalid');
	if (RESERVED_ID.test(value)) return t('idReserved');
	if (props.ids.includes(value)) return t('errExists');
	return true;
}

async function add(): Promise<void> {
	if (!(await form.value?.validate())?.valid) return;

	const app: NewApp = { id: id.value, quota: Number(quota.value) * MB };
	const trimmed = name.value.trim();
	if (trimmed) app.name = trimmed;

	busy.value = true;
	try {
		await serverApi.addApp(app);
	} catch (error) {
		message.value = errorMessage(error);
		busy.value = false;
		return;
	}
	open.value = false;
	emit('added');
}
</script>
