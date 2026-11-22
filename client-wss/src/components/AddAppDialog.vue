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
						:hint="t('appIdHint')"
						persistent-hint
						:maxlength="ID_MAX_LENGTH"
						:rules="[idRule]"
						required
						aria-labelledby=""
						autocomplete="off"
						autocapitalize="none"
						spellcheck="false"
						@beforeinput="keepIdTyped"
					>
						<!-- The asterisk is for the eye; a screen reader hears "required" from the input. Vuetify
							names the input by the label's id, whose content a screen reader reads whole, asterisk
							included; an empty aria-labelledby leaves the name to the label element, which skips it. -->
						<template #label>
							{{ t('appId') }}
							<span aria-hidden="true" class="text-error">*</span>
						</template>
					</VTextField>
					<VTextField
						v-model="name"
						:label="t('appName')"
						:hint="t('appNameHint')"
						persistent-hint
						autocomplete="off"
					/>
					<VTextField
						v-model="quota"
						inputmode="numeric"
						autocomplete="off"
						:label="t('quotaField')"
						:suffix="t('quotaUnit')"
						:hint="t(linked ? 'quotaAddLinkedHint' : 'quotaAddHint', { range })"
						persistent-hint
						:rules="[quotaRule]"
						@beforeinput="digitsOnly"
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
import { nextTick, onMounted, ref, useId, useTemplateRef, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { serverApi } from '@/api/client';
import { ID_MAX_LENGTH, ID_PATTERN, keepIdChars, RESERVED_ID } from '@/api/ids';
import type { NewApp } from '@/api/types';
import { boot } from '@/boot';
import { digitsOnly } from '@/composables/digitsOnly';
import { errorMessage, type Message } from '@/composables/messages';
import { useRules } from '@/composables/rules';
import { MB, MIN_QUOTA_MB } from '@/composables/useFormat';
import { useQuotaMax } from '@/composables/useQuotaMax';
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
const { maxMb, known, range, refresh } = useQuotaMax();
const rules = useRules();
/** The quota against the room the disk has, which is read again on each opening. */
const quotaRule = (value: string): true | string =>
	rules.wholeNumber(MIN_QUOTA_MB, maxMb.value, known.value)(value);
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
onMounted(() => void refresh());

// An empty form on each opening: what was typed and cancelled is not kept.
watch(open, async (isOpen) => {
	if (!isOpen) return;
	id.value = '';
	name.value = '';
	quota.value = defaultQuota;
	busy.value = false;
	message.value = null;
	void refresh();
	await nextTick();
	form.value?.resetValidation();
});

function focusInput(): void {
	input.value?.focus();
}

/**
 * Typing, pasting or dropping into the ID field: a capital letter lands as a
 * small one, and a character an ID cannot hold never lands in it. A pasted
 * text keeps what an ID can hold, up to the room left; the rules below still
 * catch what gets in some other way (autofill).
 */
function keepIdTyped(event: InputEvent): void {
	if (!event.inputType.startsWith('insert')) return;
	const text = event.data ?? event.dataTransfer?.getData('text/plain') ?? '';
	const kept = keepIdChars(text);
	if (kept === text) return;

	event.preventDefault();
	const field = event.target as HTMLInputElement;
	const start = field.selectionStart ?? field.value.length;
	const end = field.selectionEnd ?? start;
	const room = ID_MAX_LENGTH - (field.value.length - (end - start));
	if (kept === '' || room <= 0) return;
	field.setRangeText(kept.slice(0, room), start, end, 'end');
	field.dispatchEvent(new Event('input', { bubbles: true }));
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
