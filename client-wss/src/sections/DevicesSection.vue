<style lang="scss" module>
.center {
	justify-content: center;
}

.action {
	min-width: 150px;
}

// The heading's margins, with the information icon by it.
.title {
	display: flex;
	align-items: center;
	gap: 0.25rem;

	h2 {
		margin: 0;
	}
}

// Taller than the heading's line, which keeps its height.
.info {
	margin-block: -0.5rem;
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
			<div :class="[s.subtitle, $style.title]">
				<h2 class="text-title-large">{{ t('serversTitle') }}</h2>
				<VBtn
					:class="['text-medium-emphasis', $style.info]"
					:icon="mdiInformationOutline"
					variant="text"
					density="comfortable"
					:title="t('serversInfo')"
					:aria-label="t('serversInfo')"
					aria-haspopup="dialog"
					@click="infoOpen = true"
				/>
			</div>
			<ul :class="s.cards">
				<ServerCard
					v-for="device in servers"
					:key="device.id"
					:device="device"
					:handing-over="handingOver === device.id"
					@hand-over="handOver(device)"
					@rename="openRename('server', device.id)"
					@delete="deleteServer(device)"
				/>
			</ul>
		</div>

		<!-- The user's devices, each with the apps paired on it, after the servers. Their
		     heading only parts them from the servers: with none, the cards are the page. -->
		<div v-if="loaded">
			<h2 v-if="servers.length" :class="['text-title-large', s.subtitle]">
				{{ t('tabDevices') }}
			</h2>
			<p v-if="cards.length === 0" class="text-medium-emphasis">{{ t('noDevices') }}</p>
			<ul v-else :class="s.cards">
				<MachineCard
					v-for="card in cards"
					:key="card.id"
					:name="card.name"
					:devices="card.devices"
					:heading-tag="servers.length ? 'h3' : 'h2'"
					@apps="openApps(card.id)"
					@rename="openRename('machine', card.id)"
					@delete="deleteMachine(card)"
				/>
			</ul>
		</div>

		<InfoDialog v-model="infoOpen" :title="t('serversTitle')" :text="t('serversIntro')" />
		<PairDialog v-model="pairOpen" />
		<MachineAppsDialog
			v-model="appsOpen"
			:machine="listing?.machine ?? null"
			:devices="listing?.devices ?? []"
			:machines="cards.map((card) => card.machine)"
			:apps="apps"
			@changed="onChanged"
		/>
		<RenameDialog v-model="renameOpen" :target="renaming" @saved="onRenamed" />
		<InviteDialog v-model="inviteOpen" :main="mainName ?? ''" />
	</section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { mdiInformationOutline, mdiPlus } from '@mdi/js';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo, MachineInfo } from '@/api/types';
import InfoDialog from '@/components/InfoDialog.vue';
import MachineAppsDialog from '@/components/MachineAppsDialog.vue';
import MachineCard from '@/components/MachineCard.vue';
import RenameDialog from '@/components/RenameDialog.vue';
import type { RenameTarget } from '@/components/renameTarget';
import ServerCard from '@/components/ServerCard.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { upstreamError } from '@/composables/upstream';
import { ask } from '@/composables/useConfirm';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';
import InviteDialog from './InviteDialog.vue';
import PairDialog from './PairDialog.vue';

/**
 * The servers linked to this one, and under them the user's devices, each a
 * card with the apps paired on it. A phone with two apps is one card: the
 * server cannot tell by itself that two pairings come from one phone, so the
 * user says so when the code is issued (Pair a device asks which device it is
 * for), or later by moving a pairing from one device to another.
 */
const { t } = useI18n();

const devices = ref<DeviceInfo[]>([]);
const machines = ref<MachineInfo[]>([]);
const apps = ref<AppInfo[]>([]);
const loaded = ref(false);
const message = ref<Message | null>(null);
const infoOpen = ref(false);
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
/** The device or the linked server being renamed, by id: its copy follows the polls. */
const renamingId = ref<{ kind: 'machine' | 'server'; id: string } | null>(null);

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
const renaming = computed<RenameTarget | null>(() => {
	const { kind, id } = renamingId.value ?? {};
	if (kind === 'machine') {
		const machine = cards.value.find((card) => card.id === id)?.machine;
		if (!machine) return null;
		return { name: machine.name, rename: (name) => serverApi.renameMachine(machine.id, name) };
	}
	const device = servers.value.find((server) => server.id === id);
	if (!device) return null;
	return {
		name: device.name,
		hint: device.server && t('serverRenameHint', { name: device.server.name }),
		rename: (name) => serverApi.renameServer(device.id, name),
	};
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

function openRename(kind: 'machine' | 'server', id: string): void {
	renamingId.value = { kind, id };
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
	await deleting(() => serverApi.removeMachine(card.machine.id));
}

/** Deletes a device or a server, says so, and lists what is left. */
async function deleting(request: () => Promise<unknown>): Promise<void> {
	try {
		await request();
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

/** A linked server is deleted, after a question: the link between the two servers ends. */
async function deleteServer(device: DeviceInfo): Promise<void> {
	const question = {
		title: 'deleteMachineTitle',
		name: device.name,
		text: 'removeServerText',
		ok: 'delete',
	} as const;
	if (!(await ask(question))) return;
	await deleting(() => serverApi.removeDevice(device.id));
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
		name: device.name,
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
			values: { name: device.name },
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
</script>
