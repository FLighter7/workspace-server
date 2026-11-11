<template>
	<section aria-labelledby="devices-h">
		<h1 id="devices-h" tabindex="-1" :class="['text-headline-small', s.title]">
			{{ t('tabDevices') }}
		</h1>
		<StatusLine :message="message" />
		<p v-if="loaded && devices.length === 0" class="text-medium-emphasis">{{ t('noDevices') }}</p>

		<div v-for="group in groups" :key="group.id">
			<h2 :class="['text-title-large', s.subtitle]">
				<bdi>{{ group.name }}</bdi>
				{{ ' ' }}
				<code class="text-medium-emphasis">{{ group.id }}</code>
			</h2>
			<ul :class="s.cards">
				<ItemCard v-for="device in group.devices" :key="device.id" heading-tag="h3">
					<template #title>
						<bdi>{{ device.name }}</bdi>
					</template>
					<template #side>
						<StateBadge :tone="device.connected ? 'on' : 'off'">
							{{ device.connected ? t('connectedNow') : t('notConnected') }}
						</StateBadge>
					</template>
					<FactList>
						<FactItem :label="t('pairedAt')">{{ format.time(device.pairedAt) }}</FactItem>
						<FactItem :label="t('lastSeen')">{{ format.time(device.lastSeen) }}</FactItem>
					</FactList>
					<template #actions>
						<VBtn @click="removeDevice(device)">{{ t('remove') }}</VBtn>
					</template>
				</ItemCard>
			</ul>
		</div>
	</section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo } from '@/api/types';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import ItemCard from '@/components/ItemCard.vue';
import StateBadge from '@/components/StateBadge.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { ask } from '@/composables/useConfirm';
import { useFormat } from '@/composables/useFormat';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';

const { t } = useI18n();
const format = useFormat();

const devices = ref<DeviceInfo[]>([]);
const apps = ref<AppInfo[]>([]);
const loaded = ref(false);
const message = ref<Message | null>(null);

/** The devices by app, each group under the app's name. */
const groups = computed(() => {
	const byApp = new Map<string, DeviceInfo[]>();
	for (const device of devices.value) {
		const list = byApp.get(device.app) ?? [];
		list.push(device);
		byApp.set(device.app, list);
	}
	return [...byApp].map(([id, list]) => ({
		id,
		name: apps.value.find((app) => app.id === id)?.name ?? id,
		devices: list,
	}));
});

onMounted(async () => {
	try {
		apps.value = await serverApi.apps(); // for the groups' names
	} catch {
		// The groups then show ids instead of names.
	}
});

async function loadDevices(): Promise<void> {
	try {
		devices.value = await serverApi.devices();
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	loaded.value = true;
	if (message.value?.error) message.value = null;
}

// Keyed by id, the cards keep their elements between polls, and the focus
// stays on the button that has it.
usePolling(loadDevices);

async function removeDevice(device: DeviceInfo): Promise<void> {
	if (!(await ask({ title: 'removeTitle', name: device.name, text: 'removeText', ok: 'remove' }))) {
		return;
	}
	try {
		await serverApi.removeDevice(device.id);
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	message.value = info('removed');
	await loadDevices();
	await rescueFocus();
}
</script>
