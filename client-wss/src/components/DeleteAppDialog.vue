<style lang="scss" module>
// Keeping a copy: the advice on a faint tile, Open folder at the right of it,
// or under it when the dialog is too narrow for both on one line.
.copy {
	display: flex;
	flex-wrap: wrap;
	align-items: center;
	justify-content: flex-end;
	gap: 0.25rem 1rem;
	margin-block-start: 1rem;
	padding: 0.75rem 0.5rem 0.75rem 1rem;
	background: rgba(var(--v-theme-on-surface), 0.04);
	border-radius: 0.5rem;

	p {
		flex: 1 1 16rem;
		margin: 0;
	}
}
</style>

<template>
	<VDialog v-model="open" max-width="560" :aria-labelledby="titleId" @after-enter="focusInput">
		<VCard v-if="app" variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">
				<I18nT keypath="deleteTitle" scope="global">
					<template #name>
						<bdi>{{ app.name }}</bdi>
					</template>
				</I18nT>
			</VCardTitle>

			<VForm @submit.prevent="remove">
				<VCardText class="pt-0">
					<p class="mb-4">{{ t('deleteText') }}</p>

					<FactList>
						<FactItem :label="t('deleteSize')">{{ format.bytes(app.size) }}</FactItem>
						<FactItem :label="t('deleteDevices')">
							<template v-if="devices.length">
								<template v-for="(device, i) in devices" :key="device.id"
									><template v-if="i > 0">, </template><bdi>{{ device.name }}</bdi></template
								>
							</template>
							<template v-else>{{ t('none') }}</template>
						</FactItem>
					</FactList>

					<div v-if="app.files > 0" :class="$style.copy">
						<p class="text-body-medium">{{ t('deleteCopy') }}</p>
						<VBtn
							variant="text"
							color="primary"
							:prepend-icon="mdiFolderOpenOutline"
							:aria-label="t('openFolderLabel', { app: app.name })"
							:loading="opening"
							@click="openFolder"
						>
							{{ t('openFolder') }}
						</VBtn>
					</div>

					<!-- The control is the VTextField below, whose input carries inputId: the rule cannot see into it. -->
					<!-- eslint-disable-next-line vuejs-accessibility/label-has-for -->
					<label :for="inputId" class="d-block mt-4 mb-2">
						<I18nT keypath="typeId" scope="global">
							<template #id>
								<code>{{ app.id }}</code>
							</template>
						</I18nT>
					</label>
					<VTextField
						:id="inputId"
						ref="input"
						v-model="typed"
						density="compact"
						hide-details
						autocomplete="off"
						autocapitalize="none"
						spellcheck="false"
					/>

					<StatusLine :message="message" />
				</VCardText>

				<VCardActions>
					<VSpacer />
					<VBtn variant="text" @click="open = false">{{ t('cancel') }}</VBtn>
					<VBtn type="submit" color="error" variant="flat" :disabled="typed !== app.id || busy">
						{{ t('delete') }}
					</VBtn>
				</VCardActions>
			</VForm>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { ref, useId, useTemplateRef, watch } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { mdiFolderOpenOutline } from '@mdi/js';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo } from '@/api/types';
import { errorMessage, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { notify } from '@/composables/useNotify';
import { useReturnFocus } from '@/composables/useReturnFocus';
import FactItem from './FactItem.vue';
import FactList from './FactList.vue';
import StatusLine from './StatusLine.vue';

const props = defineProps<{
	app: AppInfo | null;
	/** The app's devices, which go with it. */
	devices: readonly DeviceInfo[];
}>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ deleted: [] }>();

const { t } = useI18n();
const format = useFormat();
const titleId = useId();
const inputId = useId();
const input = useTemplateRef<{ focus: () => void }>('input');
const typed = ref('');
const busy = ref(false);
const opening = ref(false);
const message = ref<Message | null>(null);

useReturnFocus(open);

watch(open, (isOpen) => {
	if (isOpen) {
		typed.value = '';
		busy.value = false;
		message.value = null;
	}
});

function focusInput(): void {
	input.value?.focus();
}

/**
 * "Open folder", to copy the files elsewhere before they go. The window is the
 * answer, so only an error is said, in the notification centre.
 */
async function openFolder(): Promise<void> {
	if (!props.app || opening.value) return;
	opening.value = true;
	try {
		await serverApi.openFolder(props.app.id);
	} catch (error) {
		notify(errorMessage(error));
	} finally {
		opening.value = false;
	}
}

async function remove(): Promise<void> {
	const app = props.app;
	if (!app || typed.value !== app.id) return;
	busy.value = true;
	try {
		await serverApi.deleteApp(app.id, typed.value);
	} catch (error) {
		message.value = errorMessage(error);
		busy.value = false;
		return;
	}
	open.value = false;
	emit('deleted');
}
</script>
