<style lang="scss" module>
.totals {
	margin-block-end: 1rem;
}

.usage {
	display: grid;
	gap: 0.375rem;
	max-width: 24rem;
}

// What needs the user: a divergence, a sync the quota holds, a full quota.
.marks {
	display: grid;
	gap: 0.75rem;
	margin-block-start: 0.75rem;

	p {
		margin: 0.25rem 0 0;
	}
}

.note {
	margin: 0.75rem 0 0;
}

.error {
	color: rgb(var(--v-theme-error));
}
</style>

<template>
	<section aria-labelledby="apps-h">
		<h1 id="apps-h" tabindex="-1" :class="['text-headline-small', s.title]">{{ t('tabApps') }}</h1>
		<StatusLine :message="appsMessage" />
		<p v-if="loaded && apps.length === 0" class="text-medium-emphasis">{{ t('noApps') }}</p>

		<FactList v-if="apps.length > 1" :class="$style.totals">
			<FactItem :label="t('allFiles')">{{ format.number(totalFiles) }}</FactItem>
			<FactItem :label="t('allUsed')">{{ format.bytes(totalSize) }}</FactItem>
		</FactList>

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
					<FactItem :label="t('files')">
						{{ app.files > 0 ? format.number(app.files) : t('noFiles') }}
					</FactItem>
					<FactItem :label="t('used')">
						<div :class="$style.usage">
							<span>{{ usedText(app) }}</span>
							<VProgressLinear
								:model-value="usedPercent(app)"
								:color="usageColor(app)"
								height="6"
								rounded
								:aria-label="t('quotaUsed', { app: app.name })"
								:aria-valuetext="usedText(app)"
							/>
						</div>
					</FactItem>
					<FactItem :label="t('modified')">
						{{ app.modified ? format.time(app.modified) : t('notYet') }}
					</FactItem>
					<FactItem :label="t('tabDevices')">{{ format.number(app.devices) }}</FactItem>
					<FactItem :label="t('folder')">
						<code>{{ app.folder }}</code>
					</FactItem>
				</FactList>

				<div v-if="hasMarks(app)" :class="$style.marks">
					<div v-for="other in app.diverged" :key="other.serverId">
						<StateBadge tone="warning">{{ t('divergedMark') }}</StateBadge>
						<I18nT keypath="divergedText" tag="p" scope="global">
							<template #name>
								<bdi>{{ other.name || other.serverId }}</bdi>
							</template>
						</I18nT>
					</div>
					<div v-if="app.failing === 'quota'">
						<StateBadge tone="error">{{ t('failingMark') }}</StateBadge>
						<p>{{ t('failingText') }}</p>
					</div>
					<p v-if="app.size >= app.quota" :class="$style.error">{{ t('quotaFull') }}</p>
				</div>
				<I18nT
					v-if="app.followsMain"
					keypath="quotaFollowsMain"
					tag="p"
					scope="global"
					:class="[$style.note, 'text-medium-emphasis']"
				>
					<template #name>
						<bdi>{{ mainName }}</bdi>
					</template>
				</I18nT>

				<template #actions>
					<VBtn
						:aria-label="t('openFolderLabel', { app: app.name })"
						:loading="opening === app.id"
						@click="openFolder(app)"
					>
						{{ t('openFolder') }}
					</VBtn>
					<VBtn
						v-if="!app.followsMain"
						:aria-label="t('changeQuotaLabel', { app: app.name })"
						@click="openQuota(app)"
					>
						{{ t('changeQuota') }}
					</VBtn>
				</template>
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
				v-model="newQuota"
				type="number"
				inputmode="numeric"
				:label="t('quotaField')"
				:hint="t(linked ? 'quotaAddLinkedHint' : 'quotaAddHint')"
				persistent-hint
				:rules="[quotaRule]"
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
		<QuotaDialog v-model="quotaOpen" :app="editing" @saved="onQuotaSaved" />
	</section>
</template>

<script setup lang="ts">
import { computed, ref, useTemplateRef } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { serverApi } from '@/api/client';
import { ID_PATTERN, RESERVED_ID } from '@/api/ids';
import type { AppInfo, DeviceInfo, NewApp, UpstreamStatus } from '@/api/types';
import { boot } from '@/boot';
import DeleteAppDialog from '@/components/DeleteAppDialog.vue';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import ItemCard from '@/components/ItemCard.vue';
import QuotaDialog from '@/components/QuotaDialog.vue';
import StateBadge from '@/components/StateBadge.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { useRules } from '@/composables/rules';
import { MAX_QUOTA_MB, MB, MIN_QUOTA_MB, useFormat } from '@/composables/useFormat';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';

/**
 * The apps this server serves: each with its files, the space they take of
 * its quota, its folder, and what needs the user (a divergence to choose on a
 * device, a sync the quota holds); changing the quota (not for an app a
 * subordinate follows its main for: the main sets that one), opening the
 * folder, deleting the app, and
 * adding one. The list is polled while the section is shown, so the marks
 * come and go by themselves.
 */
const { t } = useI18n();
const format = useFormat();
const quotaRule = useRules().wholeNumber(MIN_QUOTA_MB, MAX_QUOTA_MB);
const defaultQuota = String(Math.round(boot.quota / MB));

const apps = ref<AppInfo[]>([]);
const upstream = ref<UpstreamStatus | null>(null);
const loaded = ref(false);
const appsMessage = ref<Message | null>(null);
const addMessage = ref<Message | null>(null);

const addForm = useTemplateRef<VForm>('addForm');
const newId = ref('');
const newName = ref('');
const newQuota = ref(defaultQuota);

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

function usedText(app: AppInfo): string {
	return t('usedOf', { used: format.bytes(app.size), quota: format.bytes(app.quota) });
}

function usedPercent(app: AppInfo): number {
	return app.quota > 0 ? Math.min(100, Math.round((app.size / app.quota) * 100)) : 0;
}

/** The bar warns from 90 %, and is red once the files fill the quota. */
function usageColor(app: AppInfo): string {
	if (app.size >= app.quota) return 'error';
	if (app.size >= app.quota * 0.9) return 'warning';
	return 'primary';
}

function hasMarks(app: AppInfo): boolean {
	return app.diverged.length > 0 || app.failing === 'quota' || app.size >= app.quota;
}

/** The id being typed, against the pattern and the ids in use; the server checks again. */
function idRule(id: string): true | string {
	if (id === '') return t('fieldRequired');
	if (!ID_PATTERN.test(id)) return t('idInvalid');
	if (RESERVED_ID.test(id)) return t('idReserved');
	if (apps.value.some((app) => app.id === id)) return t('errExists');
	return true;
}

async function addApp(): Promise<void> {
	if (!(await addForm.value?.validate())?.valid) return;

	// On a subordinate, the main's quota replaces this one once the main
	// lists the app too; the field's hint says so.
	const app: NewApp = { id: newId.value, quota: Number(newQuota.value) * MB };
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
	newQuota.value = defaultQuota;
	addForm.value?.resetValidation();
	addMessage.value = info('added');
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
	appsMessage.value = info('deleted');
	await loadApps();
	await rescueFocus();
}
</script>
