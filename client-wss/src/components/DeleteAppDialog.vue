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
						<FactItem :label="t('size')">
							{{ app.modified ? format.bytes(app.size) : t('noDocument') }}
						</FactItem>
						<FactItem :label="t('versions')">{{ format.number(app.versions) }}</FactItem>
						<FactItem :label="t('deleteDevices')">
							<ul v-if="devices.length" :class="s.plain">
								<li v-for="device in devices" :key="device.id">
									<bdi>{{ device.name }}</bdi>
								</li>
							</ul>
							<template v-else>{{ t('none') }}</template>
						</FactItem>
					</FactList>

					<p class="my-4">
						<a v-if="app.modified" :href="documentUrl(app.id)" download>{{ t('download') }}</a>
						<template v-else>{{ t('noDocument') }}</template>
					</p>

					<!-- The control is the VTextField below, whose input carries inputId: the rule cannot see into it. -->
					<!-- eslint-disable-next-line vuejs-accessibility/label-has-for -->
					<label :for="inputId" class="d-block mb-2">
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
						{{ t('deleteForGood') }}
					</VBtn>
				</VCardActions>
			</VForm>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { ref, useId, useTemplateRef, watch } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { documentUrl, serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo } from '@/api/types';
import { errorMessage, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { useReturnFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';
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
