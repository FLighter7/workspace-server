<style lang="scss" module>
.center {
	justify-content: center;
}

.action {
	min-width: 150px;
}
</style>

<template>
	<section aria-labelledby="devices-h">
		<!-- Not drawn: the header's tab names the page. It stays for a screen reader, and as the
			focus's fallback when a removed device's button leaves. -->
		<h1 id="devices-h" tabindex="-1" class="d-sr-only">{{ t('tabDevices') }}</h1>
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
			<div :class="[s.actions, $style.center]">
				<VBtn
					:class="$style.action"
					color="primary"
					variant="flat"
					:prepend-icon="mdiPlus"
					@click="inviteOpen = true"
				>
					{{ t('inviteButton') }}
				</VBtn>
			</div>
		</div>
		<div v-else-if="upstreamLoaded" :class="[s.actions, $style.center, 'mb-4']">
			<VBtn
				:class="$style.action"
				:aria-label="t('pairDevice')"
				color="primary"
				variant="flat"
				:prepend-icon="mdiPlus"
				@click="pairOpen = true"
			>
				{{ t('pair') }}
			</VBtn>
		</div>
		<StatusLine :message="message" />

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

		<!-- The user's devices, each with the apps paired on it, after the servers. -->
		<div v-if="loaded">
			<h2 :class="['text-title-large', s.subtitle]">{{ t('tabDevices') }}</h2>
			<p v-if="cards.length === 0" class="text-medium-emphasis">{{ t('noDevices') }}</p>
			<ul v-else :class="s.cards">
				<MachineCard
					v-for="card in cards"
					:key="card.id"
					:name="card.name"
					:devices="card.devices"
					@apps="openApps(card.id)"
					@rename="openRename(card.id)"
					@delete="deleteMachine(card)"
				/>
			</ul>
		</div>

		<PairDialog v-model="pairOpen" />
		<MachineAppsDialog
			v-model="appsOpen"
			:machine="listing?.machine ?? null"
			:devices="listing?.devices ?? []"
			:machines="cards.map((card) => card.machine)"
			:apps="apps"
			@changed="onChanged"
		/>
		<RenameMachineDialog v-model="renameOpen" :machine="renaming" @saved="onRenamed" />
		<InviteDialog v-model="inviteOpen" :main="mainName ?? ''" />
	</section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { mdiPlus } from '@mdi/js';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo, MachineInfo } from '@/api/types';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import ItemCard from '@/components/ItemCard.vue';
import MachineAppsDialog from '@/components/MachineAppsDialog.vue';
import MachineCard from '@/components/MachineCard.vue';
import RenameMachineDialog from '@/components/RenameMachineDialog.vue';
import StateBadge from '@/components/StateBadge.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { upstreamError } from '@/composables/upstream';
import { ask, type Question } from '@/composables/useConfirm';
import { useFormat } from '@/composables/useFormat';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';
import InviteDialog from './InviteDialog.vue';
import PairDialog from './PairDialog.vue';

/**
 * The user's devices, each a card with the apps paired on it, and under them
 * the servers linked to this one. A phone with two apps is one card: the
 * server cannot tell by itself that two pairings come from one phone, so the
 * user says so when the code is issued (Pair a device asks which device it is
 * for), or later by moving a pairing from one device to another.
 */
const { t } = useI18n();
const format = useFormat();

const devices = ref<DeviceInfo[]>([]);
const machines = ref<MachineInfo[]>([]);
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
const appsOpen = ref(false);
/** The device whose apps the dialog lists, by id: its copy follows the polls. */
const listingId = ref<string | null>(null);
const renameOpen = ref(false);
const renamingId = ref<string | null>(null);

/** The servers linked to this one: its subordinates. */
const servers = computed(() => devices.value.filter((device) => device.kind === 'server'));

/** One of the user's devices, with the pairings of the apps on it. */
interface Card {
	id: string;
	name: string;
	machine: MachineInfo;
	devices: DeviceInfo[];
}

/**
 * The devices of apps by the machine each is paired on, by name. A pairing
 * the server names no machine for stands alone, under its own name.
 */
const cards = computed<Card[]>(() => {
	const byMachine = new Map<string, DeviceInfo[]>();
	for (const device of devices.value) {
		if (device.kind === 'server') continue;
		const id = device.machine ?? device.id;
		byMachine.set(id, [...(byMachine.get(id) ?? []), device]);
	}
	return [...byMachine]
		.map(([id, list]) => {
			const name = machines.value.find((m) => m.id === id)?.name ?? list[0].name;
			return { id, name, machine: { id, name }, devices: list };
		})
		.sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
});

const listing = computed(() => cards.value.find((card) => card.id === listingId.value) ?? null);
const renaming = computed(
	() => cards.value.find((card) => card.id === renamingId.value)?.machine ?? null,
);

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
	void loadMachines();
	try {
		devices.value = await serverApi.devices();
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	loaded.value = true;
	if (message.value?.error) message.value = null;
}

/** The machines' names; a failure keeps the last, or shows each pairing's own name. */
async function loadMachines(): Promise<void> {
	try {
		machines.value = await serverApi.machines();
	} catch {
		// The next poll tries again.
	}
}

function openApps(id: string): void {
	listingId.value = id;
	appsOpen.value = true;
}

function openRename(id: string): void {
	renamingId.value = id;
	renameOpen.value = true;
}

/** A pairing moved or removed in the apps dialog: the cards follow at once. */
async function onChanged(): Promise<void> {
	await loadDevices();
	if (!appsOpen.value) await rescueFocus();
}

async function onRenamed(): Promise<void> {
	message.value = info('renamed');
	await loadDevices();
}

/** A device is deleted with every app on it, after a question. */
async function deleteMachine(card: Card): Promise<void> {
	const question = {
		title: 'deleteMachineTitle',
		name: card.name,
		text: 'disconnectMachineText',
		ok: 'delete',
	} as const;
	if (!(await ask(question))) return;
	try {
		await serverApi.removeMachine(card.machine.id);
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	message.value = info('deleted');
	await loadDevices();
	await rescueFocus();
}

// Keyed by id, the cards keep their elements between polls, and the focus
// stays on the button that has it.
usePolling(loadDevices);

/** A linked server is removed: the link between the two servers ends. */
async function removeServer(device: DeviceInfo): Promise<void> {
	const question = {
		title: 'removeTitle',
		name: device.server?.name ?? device.name,
		text: 'removeServerText',
		ok: 'remove',
	} as const;
	await remove(device, question, 'removed');
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

async function remove(device: DeviceInfo, question: Question, done: 'removed'): Promise<void> {
	if (!(await ask(question))) return;
	try {
		await serverApi.removeDevice(device.id);
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	message.value = info(done);
	await loadDevices();
	await rescueFocus();
}
</script>
