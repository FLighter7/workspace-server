<template>
	<section aria-labelledby="devices-h">
		<h1 id="devices-h" tabindex="-1" :class="['text-headline-small', s.title]">
			{{ t('tabDevices') }}
		</h1>
		<!-- A subordinate pairs nothing: devices pair with its main, and a
		     browser on this computer gets a code from the main through it. -->
		<div v-if="mainName !== null" class="mb-4">
			<p class="mb-3">
				<I18nT keypath="subordinateDevices" scope="global">
					<template #name>
						<bdi>{{ mainName }}</bdi>
					</template>
				</I18nT>
			</p>
			<div :class="s.actions">
				<VBtn color="primary" variant="flat" @click="inviteOpen = true">
					{{ t('inviteButton') }}
				</VBtn>
			</div>
		</div>
		<div v-else-if="upstreamLoaded" :class="[s.actions, 'mb-4']">
			<VBtn color="primary" variant="flat" @click="pairOpen = true">{{ t('pairDevice') }}</VBtn>
		</div>
		<StatusLine :message="message" />
		<p v-if="loaded && devices.length === 0" class="text-medium-emphasis">{{ t('noDevices') }}</p>

		<div v-if="servers.length">
			<h2 :class="['text-title-large', s.subtitle]">{{ t('serversTitle') }}</h2>
			<p class="text-medium-emphasis mb-3">{{ t('serversIntro') }}</p>
			<ul :class="s.cards">
				<ItemCard v-for="device in servers" :key="device.id" heading-tag="h3">
					<template #title>
						<bdi>{{ device.server?.name ?? device.name }}</bdi>
					</template>
					<template #side>
						<StateBadge :tone="device.connected ? 'on' : 'off'">
							{{ device.connected ? t('connectedNow') : t('notConnected') }}
						</StateBadge>
					</template>
					<FactList>
						<template v-if="device.server">
							<FactItem :label="t('serverId')">
								<code>{{ device.server.serverId }}</code>
							</FactItem>
							<FactItem :label="t('addresses')">
								<ul :class="s.plain">
									<li v-for="host in device.server.hosts" :key="host">
										<code>{{ host }}</code>
									</li>
								</ul>
							</FactItem>
							<FactItem :label="t('udpPort')">{{ device.server.port }}</FactItem>
						</template>
						<FactItem v-else :label="t('addresses')">{{ t('serverNotAnnounced') }}</FactItem>
						<FactItem :label="t('pairedAt')">{{ format.time(device.pairedAt) }}</FactItem>
						<FactItem :label="t('lastSeen')">{{ format.time(device.lastSeen) }}</FactItem>
					</FactList>
					<template #actions>
						<VBtn
							v-if="device.server"
							:loading="handingOver === device.id"
							@click="handOver(device)"
						>
							{{ t('handOverButton') }}
						</VBtn>
						<VBtn @click="removeServer(device)">{{ t('remove') }}</VBtn>
					</template>
				</ItemCard>
			</ul>
		</div>

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

		<PairDialog v-model="pairOpen" />
		<InviteDialog v-model="inviteOpen" :main="mainName ?? ''" />
	</section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo } from '@/api/types';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import ItemCard from '@/components/ItemCard.vue';
import StateBadge from '@/components/StateBadge.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { upstreamError } from '@/composables/upstream';
import { ask } from '@/composables/useConfirm';
import { useFormat } from '@/composables/useFormat';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';
import InviteDialog from './InviteDialog.vue';
import PairDialog from './PairDialog.vue';

const { t } = useI18n();
const format = useFormat();

const devices = ref<DeviceInfo[]>([]);
const apps = ref<AppInfo[]>([]);
const loaded = ref(false);
const message = ref<Message | null>(null);
const pairOpen = ref(false);
const inviteOpen = ref(false);
/** The main's name while this server is its subordinate; null on a main. */
const mainName = ref<string | null>(null);
const upstreamLoaded = ref(false);
/** The server-kind device the role is being handed to. */
const handingOver = ref<string | null>(null);

/** The servers linked to this one: its subordinates. */
const servers = computed(() => devices.value.filter((device) => device.kind === 'server'));

/** The devices of apps by app, each group under the app's name. */
const groups = computed(() => {
	const byApp = new Map<string, DeviceInfo[]>();
	for (const device of devices.value) {
		if (device.kind === 'server') continue;
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

async function loadUpstream(): Promise<void> {
	try {
		const upstream = await serverApi.upstream();
		mainName.value = upstream.linked ? (upstream.name ?? '') : null;
	} catch {
		// Shown as a main: a subordinate's own pairing codes are refused with
		// their own message.
	}
	upstreamLoaded.value = true;
}

onMounted(loadUpstream);

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
	await remove(device, device.name, 'removeText');
}

async function removeServer(device: DeviceInfo): Promise<void> {
	await remove(device, device.server?.name ?? device.name, 'removeServerText');
}

/**
 * "Make it the main" on a linked server: this server hands the role of main
 * to it, and then syncs through it.
 */
async function handOver(device: DeviceInfo): Promise<void> {
	const server = device.server;
	if (!server || handingOver.value !== null) return;
	const question = {
		title: 'handOverTitle',
		name: server.name,
		text: 'handOverText',
		ok: 'handOverOk',
	} as const;
	if (!(await ask(question))) return;
	handingOver.value = device.id;
	message.value = info('handingOver');
	try {
		const result = await serverApi.handOver(server.serverId);
		message.value = {
			key: result.done ? 'handedOver' : 'handOverPending',
			values: { name: server.name },
		};
	} catch (error) {
		message.value = upstreamError(error);
		return;
	} finally {
		handingOver.value = null;
	}
	await loadUpstream();
	await loadDevices();
	await rescueFocus();
}

async function remove(
	device: DeviceInfo,
	name: string,
	text: 'removeText' | 'removeServerText',
): Promise<void> {
	if (!(await ask({ title: 'removeTitle', name, text, ok: 'remove' }))) return;
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
