<template>
	<VDialog v-model="open" max-width="560" :aria-labelledby="titleId" @after-enter="focusInput">
		<VCard v-if="target" variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">
				<I18nT keypath="renameTitle" scope="global">
					<template #name>
						<bdi>{{ target.name }}</bdi>
					</template>
				</I18nT>
			</VCardTitle>

			<VForm ref="form" @submit.prevent="save">
				<VCardText class="pt-0">
					<VTextField
						ref="input"
						v-model="name"
						:label="t('machineName')"
						autocomplete="off"
						:counter="64"
						maxlength="64"
						:rules="[rules.required]"
						:hint="target.hint"
						persistent-hint
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
import { ref, useId, useTemplateRef, watch } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { errorMessage, type Message } from '@/composables/messages';
import { useRules } from '@/composables/rules';
import { useReturnFocus } from '@/composables/useReturnFocus';
import type { RenameTarget } from './renameTarget';
import StatusLine from './StatusLine.vue';

/**
 * A new name for one of the user's devices ("Alex's phone" instead of the
 * "iPhone" it took from the first app paired on it) or for a linked server,
 * whose own name a hint under the field gives. The name is the server's, so
 * every page of the family shows it.
 */
const props = defineProps<{ target: RenameTarget | null }>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ saved: [] }>();

const { t } = useI18n();
const rules = useRules();
const titleId = useId();
const form = useTemplateRef<VForm>('form');
const input = useTemplateRef<{ focus: () => void }>('input');
const name = ref('');
const busy = ref(false);
const message = ref<Message | null>(null);

useReturnFocus(open);

// Filled once on opening: the list behind the dialog is polled, and a new
// copy of the device must not undo what is being typed.
watch(open, (isOpen) => {
	if (!isOpen) return;
	name.value = props.target?.name ?? '';
	busy.value = false;
	message.value = null;
});

function focusInput(): void {
	input.value?.focus();
}

async function save(): Promise<void> {
	const target = props.target;
	if (!target) return;
	if (name.value.trim() === target.name) {
		open.value = false;
		return;
	}
	if (!(await form.value?.validate())?.valid) return;
	busy.value = true;
	try {
		await target.rename(name.value.trim());
	} catch (error) {
		message.value = errorMessage(error);
		busy.value = false;
		return;
	}
	open.value = false;
	emit('saved');
}
</script>
