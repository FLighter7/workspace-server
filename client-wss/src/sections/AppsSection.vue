<template>
	<section aria-labelledby="apps-h">
		<h1 id="apps-h" tabindex="-1" :class="['text-headline-small', s.title]">{{ t('tabApps') }}</h1>
		<StatusLine :message="appsMessage" />
		<p v-if="loaded && apps.length === 0" class="text-medium-emphasis">{{ t('noApps') }}</p>

		<ul :class="s.cards">
			<ItemCard v-for="app in apps" :key="app.id">
				<template #title>
					<bdi>{{ app.name }}</bdi>
					{{ ' ' }}
					<code class="text-medium-emphasis">{{ app.id }}</code>
				</template>
				<template #side>
					<VBtn color="error" @click="openDelete(app)">{{ t('delete') }}</VBtn>
				</template>
				<FactList>
					<FactItem :label="t('size')">
						{{ app.modified ? format.bytes(app.size) : t('noDocument') }}
					</FactItem>
					<FactItem :label="t('modified')">
						{{ app.modified ? format.time(app.modified) : t('noDocument') }}
					</FactItem>
					<FactItem :label="t('versions')">{{ format.number(app.versions) }}</FactItem>
					<FactItem :label="t('tabDevices')">{{ format.number(app.devices) }}</FactItem>
				</FactList>
			</ItemCard>
		</ul>

		<h2 :class="['text-title-large', s.subtitle]">{{ t('addApp') }}</h2>
		<VForm ref="addForm" :class="s.form" @submit.prevent="addApp">
			<VTextField
				v-model="newId"
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
				v-model="newName"
				:label="t('appName')"
				:hint="t('appNameHint')"
				persistent-hint
				autocomplete="off"
			/>
			<VTextField
				v-model="history"
				type="number"
				inputmode="numeric"
				:label="t('history')"
				:rules="[historyRule]"
			/>
			<VTextField
				v-model="maxBytes"
				type="number"
				inputmode="numeric"
				:label="t('maxBytes')"
				:rules="[maxBytesRule]"
			/>
			<div :class="s.actions">
				<VBtn type="submit" color="primary" variant="flat">{{ t('add') }}</VBtn>
			</div>
			<StatusLine :message="addMessage" />
		</VForm>

		<DeleteAppDialog
			v-model="deleteOpen"
			:app="deleting"
			:devices="deletingDevices"
			@deleted="onDeleted"
		/>
	</section>
</template>

<script setup lang="ts">
import { onMounted, ref, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { serverApi } from '@/api/client';
import { ID_PATTERN } from '@/api/ids';
import type { AppInfo, DeviceInfo, NewApp } from '@/api/types';
import { boot } from '@/boot';
import DeleteAppDialog from '@/components/DeleteAppDialog.vue';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import ItemCard from '@/components/ItemCard.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { useRules } from '@/composables/rules';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';

const { t } = useI18n();
const format = useFormat();
const rules = useRules();
const historyRule = rules.wholeNumber(1, 100);
const maxBytesRule = rules.wholeNumber(1024, 1_073_741_824);

const apps = ref<AppInfo[]>([]);
const loaded = ref(false);
const appsMessage = ref<Message | null>(null);
const addMessage = ref<Message | null>(null);

const addForm = useTemplateRef<VForm>('addForm');
const newId = ref('');
const newName = ref('');
const history = ref(String(boot.history));
const maxBytes = ref(String(boot.maxBytes));

const deleteOpen = ref(false);
const deleting = ref<AppInfo | null>(null);
const deletingDevices = ref<DeviceInfo[]>([]);

async function loadApps(): Promise<void> {
	try {
		apps.value = await serverApi.apps();
	} catch (error) {
		appsMessage.value = errorMessage(error);
		return;
	}
	loaded.value = true;
}

onMounted(loadApps);

/** The id being typed, against the pattern and the ids in use; the server checks again. */
function idRule(id: string): true | string {
	if (id === '') return t('fieldRequired');
	if (!ID_PATTERN.test(id)) return t('idInvalid');
	if (apps.value.some((app) => app.id === id)) return t('errExists');
	return true;
}

async function addApp(): Promise<void> {
	if (!(await addForm.value?.validate())?.valid) return;

	const app: NewApp = {
		id: newId.value,
		history: Number(history.value),
		maxBytes: Number(maxBytes.value),
	};
	const name = newName.value.trim();
	if (name) app.name = name;

	try {
		await serverApi.addApp(app);
	} catch (error) {
		addMessage.value = errorMessage(error);
		return;
	}

	newId.value = '';
	newName.value = '';
	history.value = String(boot.history);
	maxBytes.value = String(boot.maxBytes);
	addForm.value?.resetValidation();
	addMessage.value = info('added');
	await loadApps();
}

async function openDelete(app: AppInfo): Promise<void> {
	try {
		deletingDevices.value = (await serverApi.devices()).filter((d) => d.app === app.id);
	} catch (error) {
		appsMessage.value = errorMessage(error);
		return;
	}
	deleting.value = app;
	deleteOpen.value = true;
}

async function onDeleted(): Promise<void> {
	appsMessage.value = info('deleted');
	await loadApps();
	await rescueFocus();
}
</script>
