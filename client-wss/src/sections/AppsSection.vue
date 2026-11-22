<style lang="scss" module>
.add {
	display: flex;
	min-width: 150px;
	margin-inline: auto;
	margin-block-end: 1rem;
}

.totals {
	margin-block-end: 1rem;
}
</style>

<template>
	<section aria-labelledby="apps-h">
		<!-- Not drawn: the header's tab names the page. It stays for a screen reader, and as the
			focus's fallback when a deleted app's button leaves. -->
		<h1 id="apps-h" tabindex="-1" class="d-sr-only">{{ t('tabApps') }}</h1>
		<VBtn
			:class="$style.add"
			:aria-label="t('addApp')"
			color="primary"
			variant="flat"
			:prepend-icon="mdiPlus"
			@click="addOpen = true"
		>
			{{ t('add') }}
		</VBtn>
		<StatusLine :message="appsMessage" />
		<p v-if="loaded && apps.length === 0" class="text-medium-emphasis">{{ t('noApps') }}</p>

		<FactList v-if="apps.length > 1" :class="$style.totals">
			<FactItem :label="t('allFiles')">{{ format.number(totalFiles) }}</FactItem>
			<FactItem :label="t('allUsed')">{{ format.bytes(totalSize) }}</FactItem>
		</FactList>

		<ul :class="s.cards">
			<AppCard
				v-for="app in apps"
				:key="app.id"
				:app="app"
				:main-name="mainName"
				:opening="opening === app.id"
				@open-folder="openFolder(app)"
				@quota="openQuota(app)"
				@delete="openDelete(app)"
			/>
		</ul>

		<!-- What was done to an app, said once and gone: the list shows the rest. -->
		<VSnackbar v-model="toastOpen" color="success" location="bottom end">{{
			toast ? t(toast) : ''
		}}</VSnackbar>
		<AddAppDialog
			v-model="addOpen"
			:ids="apps.map((app) => app.id)"
			:linked="linked"
			@added="onAdded"
		/>
		<DeleteAppDialog
			v-model="deleteOpen"
			:app="deleting"
			:devices="deletingDevices"
			@deleted="onDeleted"
		/>
		<QuotaDialog v-model="quotaOpen" :app="editing" @saved="onQuotaSaved" />
	</section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiPlus } from '@mdi/js';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo, UpstreamStatus } from '@/api/types';
import AddAppDialog from '@/components/AddAppDialog.vue';
import AppCard from '@/components/AppCard.vue';
import DeleteAppDialog from '@/components/DeleteAppDialog.vue';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import QuotaDialog from '@/components/QuotaDialog.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';

/**
 * The apps this server serves, one card each (AppCard): its files, the space
 * they take of its quota, its folder, and what needs the user (a divergence
 * to choose on a device, a sync the quota holds). This section does what the
 * cards ask: changing the quota (not for an app a subordinate follows its main
 * for: the main sets that one), opening the folder, deleting the app, and
 * adding one. The list is polled while the section is shown, so the marks
 * come and go by themselves.
 */
const { t } = useI18n();
const format = useFormat();

const apps = ref<AppInfo[]>([]);
const upstream = ref<UpstreamStatus | null>(null);
const loaded = ref(false);
const appsMessage = ref<Message | null>(null);

const addOpen = ref(false);
/** The outcome of adding or deleting an app, shown in a snackbar. */
const toast = ref<'added' | 'deleted' | null>(null);
const toastOpen = ref(false);

const deleteOpen = ref(false);
const deleting = ref<AppInfo | null>(null);
const deletingDevices = ref<DeviceInfo[]>([]);

const quotaOpen = ref(false);
const editing = ref<AppInfo | null>(null);
/** The app whose folder is being opened. */
const opening = ref<string | null>(null);

/**
 * A subordinate: it takes the quota of each app its main lists from the main
 * (AppInfo.followsMain); an app only it serves is its own.
 */
const linked = computed(() => upstream.value?.linked === true);
const mainName = computed(() => upstream.value?.name ?? '');
const totalFiles = computed(() => apps.value.reduce((sum, app) => sum + app.files, 0));
const totalSize = computed(() => apps.value.reduce((sum, app) => sum + app.size, 0));

/**
 * The apps, and the link for the main's name. The link is read on its own:
 * a failure there leaves the list as it comes. An error shown by an earlier
 * poll goes once the server answers again.
 */
async function loadApps(): Promise<void> {
	void loadUpstream();
	try {
		apps.value = await serverApi.apps();
	} catch (error) {
		appsMessage.value = errorMessage(error);
		return;
	}
	loaded.value = true;
	if (appsMessage.value?.error) appsMessage.value = null;
}

async function loadUpstream(): Promise<void> {
	try {
		upstream.value = await serverApi.upstream();
	} catch {
		// The list says what matters; the main's name waits for the next poll.
	}
}

usePolling(loadApps);

function say(outcome: 'added' | 'deleted'): void {
	toast.value = outcome;
	toastOpen.value = true;
}

async function onAdded(): Promise<void> {
	say('added');
	await loadApps();
}

/**
 * "Open folder": the server opens the app's folder in the file manager of its
 * own computer, which is the one this page is open on.
 */
async function openFolder(app: AppInfo): Promise<void> {
	if (opening.value !== null) return;
	opening.value = app.id;
	try {
		await serverApi.openFolder(app.id);
		appsMessage.value = { key: 'folderOpened', values: { app: app.name } };
	} catch (error) {
		appsMessage.value = errorMessage(error);
	} finally {
		opening.value = null;
	}
}

function openQuota(app: AppInfo): void {
	editing.value = app;
	quotaOpen.value = true;
}

async function onQuotaSaved(quota: number): Promise<void> {
	const app = editing.value;
	if (app) {
		appsMessage.value = {
			key: 'quotaSaved',
			values: { app: app.name, quota: format.bytes(quota) },
		};
	}
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
	say('deleted');
	await loadApps();
	await rescueFocus();
}
</script>
