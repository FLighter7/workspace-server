<style lang="scss" module>
// The UDP port's hint warns: every device has to pair again after a change.
.warning :global(.v-messages) {
	color: rgb(var(--v-theme-warning));
	opacity: 1;
}

// The danger zone, set apart by the theme's error colour.
.danger {
	min-width: 0;
	margin: 2rem 0 0;
	padding: 0.5rem 1rem 1rem;
	border: 1px solid rgb(var(--v-theme-error));
	border-radius: 0.5rem;
}

.legend {
	padding-inline: 0.5rem;

	h2 {
		margin: 0;
	}
}
</style>

<template>
	<section aria-labelledby="server-h">
		<h1 id="server-h" tabindex="-1" :class="['text-headline-small', s.title]">
			{{ t('tabServer') }}
		</h1>
		<StatusLine :message="serverMessage" />

		<FactList v-if="status">
			<FactItem :label="t('serverId')">
				<code>{{ status.serverId }}</code>
			</FactItem>
			<FactItem :label="t('addresses')">
				<ul v-if="status.addresses.length" :class="s.plain">
					<li v-for="address in status.addresses" :key="address">
						<code>{{ address }}</code>
					</li>
				</ul>
				<template v-else>{{ t('noAddresses') }}</template>
				<template v-if="!status.listening">
					<br />
					<StateBadge tone="error">{{ t('notListening') }}</StateBadge>
				</template>
			</FactItem>
			<FactItem :label="t('mdnsName')">
				<code>{{ status.mdnsName }}</code>
				<br />
				<StateBadge :tone="status.mdnsRunning ? 'on' : 'off'">
					{{ status.mdnsRunning ? t('mdnsUp') : t('mdnsDown') }}
				</StateBadge>
			</FactItem>
		</FactList>

		<VForm ref="serverForm" :class="s.form" @submit.prevent="saveServer">
			<VTextField
				v-model="name"
				:label="t('serverName')"
				:hint="t('serverNameHint')"
				persistent-hint
				:rules="[rules.required]"
				autocomplete="off"
			/>
			<VTextField
				v-model="udpPort"
				:class="$style.warning"
				type="number"
				inputmode="numeric"
				:label="t('udpPort')"
				:hint="t('udpWarning')"
				persistent-hint
				:rules="[portRule]"
			/>
			<VTextField
				v-model="httpPort"
				type="number"
				inputmode="numeric"
				:label="t('httpPort')"
				:hint="t('httpHint')"
				persistent-hint
				:rules="[portRule]"
			/>
			<div :class="s.actions">
				<VBtn type="submit" color="primary" variant="flat">{{ t('save') }}</VBtn>
			</div>
		</VForm>

		<VForm ref="dataForm" :class="[s.form, s.wide]" @submit.prevent="changeDataDir">
			<VTextField
				v-model="dataDir"
				:label="t('dataDirLabel')"
				:hint="t('dataDirHint')"
				persistent-hint
				:rules="[rules.required]"
				autocomplete="off"
				autocapitalize="none"
				spellcheck="false"
				@focus="dataDirFocused = true"
				@blur="dataDirFocused = false"
			/>
			<div :class="s.actions">
				<VBtn type="submit">{{ t('dataDirChange') }}</VBtn>
			</div>
			<StatusLine :message="dataMessage" />
		</VForm>

		<!-- The danger zone: every button here stops or wipes the server. -->
		<fieldset :class="$style.danger">
			<legend :class="$style.legend">
				<h2 class="text-title-large">{{ t('manageTitle') }}</h2>
			</legend>
			<p class="text-medium-emphasis mb-4">{{ t('manageIntro') }}</p>
			<div :class="s.actions">
				<VBtn @click="stopServer">{{ t('stopButton') }}</VBtn>
				<VBtn color="error" @click="resetData">{{ t('resetButton') }}</VBtn>
				<VBtn color="error" @click="deleteServer">{{ t('deleteServerButton') }}</VBtn>
			</div>
			<StatusLine :message="manageMessage" />
		</fieldset>

		<WipeDialog v-model="wipeOpen" :wipe="wipe" />
	</section>
</template>

<script setup lang="ts">
import { onMounted, ref, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import type { VForm } from 'vuetify/components';
import { ApiError, serverApi } from '@/api/client';
import type { DataDirCheck, Status } from '@/api/types';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import StateBadge from '@/components/StateBadge.vue';
import StatusLine from '@/components/StatusLine.vue';
import WipeDialog from '@/components/WipeDialog.vue';
import type { Wipe, WipeFact } from '@/components/wipe';
import { errorMessage, info, type Message } from '@/composables/messages';
import { ask } from '@/composables/useConfirm';
import { dropCode } from '@/composables/usePairingCode';
import { waitRestart } from '@/composables/useRestart';
import { useRules } from '@/composables/rules';
import { showStopped } from '@/composables/useSection';
import s from '@/styles/section.module.scss';

const { t } = useI18n();
const rules = useRules();
const portRule = rules.wholeNumber(1, 65535);

const status = ref<Status | null>(null);
const name = ref('');
const udpPort = ref('');
const httpPort = ref('');
const dataDir = ref('');
const dataDirFocused = ref(false);

const serverMessage = ref<Message | null>(null);
const dataMessage = ref<Message | null>(null);
const manageMessage = ref<Message | null>(null);

const serverForm = useTemplateRef<VForm>('serverForm');
const dataForm = useTemplateRef<VForm>('dataForm');

const wipeOpen = ref(false);
const wipe = ref<Wipe | null>(null);

function confirmWipe(question: Wipe): void {
	wipe.value = question;
	wipeOpen.value = true;
}

async function loadStatus(): Promise<void> {
	let loaded: Status;
	try {
		loaded = await serverApi.status();
	} catch (error) {
		serverMessage.value = errorMessage(error);
		return;
	}

	status.value = loaded;
	name.value = loaded.name;
	udpPort.value = String(loaded.udpPort);
	httpPort.value = String(loaded.httpPort);
	if (!dataDirFocused.value) dataDir.value = loaded.dataDir;
	if (loaded.lastError) {
		serverMessage.value = { key: 'restartError', values: { error: loaded.lastError }, error: true };
	}
}

onMounted(loadStatus);

// ---- Name and ports --------------------------------------------------------

async function saveServer(): Promise<void> {
	const current = status.value;
	if (!current || !(await serverForm.value?.validate())?.valid) return;

	const change: { name?: string; udpPort?: number; httpPort?: number } = {};
	const newName = name.value.trim();
	const udp = Number(udpPort.value);
	const http = Number(httpPort.value);
	if (newName !== current.name) change.name = newName;
	if (udp !== current.udpPort) change.udpPort = udp;
	if (http !== current.httpPort) change.httpPort = http;

	if (Object.keys(change).length === 0) {
		serverMessage.value = info('saved');
		return;
	}

	if (
		change.udpPort !== undefined &&
		!(await ask({ title: 'confirmUdpTitle', text: 'confirmUdpText', ok: 'confirmUdpButton' }))
	) {
		return;
	}

	try {
		await serverApi.setConfig(change);
	} catch (error) {
		serverMessage.value = errorMessage(error);
		return;
	}

	// A code on screen carries the old port and name; the pairing dialog
	// issues a new one the next time it opens.
	if (change.udpPort !== undefined || change.name !== undefined) dropCode();

	if (change.httpPort !== undefined) {
		// The server now answers on the new port only; the browser follows.
		const url = `http://${location.hostname}:${change.httpPort}/#server`;
		serverMessage.value = { key: 'movedTo', links: { url } };
		setTimeout(() => location.assign(url), 1500);
		return;
	}

	serverMessage.value = info('saved');
	await loadStatus();
}

// ---- The data folder -------------------------------------------------------

async function changeDataDir(): Promise<void> {
	if (!status.value || !(await dataForm.value?.validate())?.valid) return;

	let check: DataDirCheck;
	try {
		check = await serverApi.checkDataDir(dataDir.value.trim());
	} catch (error) {
		dataMessage.value = errorMessage(error);
		return;
	}

	const target = check.path;
	const say = (message: Message): void => {
		dataMessage.value = message;
	};
	const apply = async (): Promise<void> => {
		const done = await serverApi.setDataDir(target, check.verdict);
		if (done === null) {
			say(info('saved')); // the current folder
			return;
		}
		void waitRestart(say, (st) => st.dataDir === target || st.lastError !== '');
	};
	const run = async (): Promise<void> => {
		try {
			await apply();
		} catch (error) {
			say(
				error instanceof ApiError && error.code === 'invalid'
					? { key: 'errDataInvalid', detail: error.message, error: true }
					: errorMessage(error),
			);
		}
	};
	const folder: WipeFact = { kind: 'path', label: 'dataDirFact', path: target };

	switch (check.verdict) {
		case 'same':
			say(info('saved'));
			return;
		case 'conflict':
			say({ key: 'errDataExists', error: true });
			return;
		case 'unavailable':
			say({ key: 'errDataInvalid', detail: check.message, error: true });
			return;
		case 'adopt':
			confirmWipe({
				title: 'adoptTitle',
				text: 'adoptText',
				ok: 'adoptOk',
				danger: false,
				facts: [folder, { kind: 'apps', apps: check.target, download: false }],
				action: apply,
			});
			return;
		case 'move':
		case 'create':
			if (check.current.length > 0) {
				confirmWipe({
					title: 'moveTitle',
					text: 'moveText',
					ok: 'moveOk',
					danger: false,
					facts: [folder, { kind: 'apps', apps: check.current, download: false }],
					action: apply,
				});
				return;
			}
			break;
	}

	await run(); // switch, or create with nothing to move: no question
}

// ---- Managing the server ---------------------------------------------------

async function stopServer(): Promise<void> {
	if (!(await ask({ title: 'stopTitle', text: 'stopText', ok: 'stopOk' }))) return;
	try {
		await serverApi.stop();
	} catch (error) {
		manageMessage.value = errorMessage(error);
		return;
	}
	showStopped('stoppedText');
}

/** What a reset or a deletion removes, and the status it was read with. */
async function wipeFacts(withFolders: boolean): Promise<{ status: Status; facts: WipeFact[] }> {
	const [st, apps, devices] = await Promise.all([
		serverApi.status(),
		serverApi.apps(),
		serverApi.devices(),
	]);
	const facts: WipeFact[] = [
		{ kind: 'apps', apps, download: true },
		{ kind: 'count', label: 'devicesFact', value: devices.length },
	];
	if (withFolders) {
		facts.push({ kind: 'path', label: 'serverDirFact', path: st.serverDir });
		if (st.dataDir !== st.serverDir) {
			facts.push({ kind: 'path', label: 'dataDirFact', path: st.dataDir });
		}
		if (st.mainServer && st.program) {
			facts.push({ kind: 'path', label: 'programFact', path: st.program });
		}
	}

	return { status: st, facts };
}

async function resetData(): Promise<void> {
	let gathered: Awaited<ReturnType<typeof wipeFacts>>;
	try {
		gathered = await wipeFacts(false);
	} catch (error) {
		manageMessage.value = errorMessage(error);
		return;
	}

	confirmWipe({
		title: 'resetTitle',
		text: 'resetText',
		ok: 'resetOk',
		danger: true,
		facts: gathered.facts,
		action: async () => {
			await serverApi.reset();
			dropCode();
			void waitRestart(
				(message) => {
					manageMessage.value = message;
				},
				(st) => st.serverId !== gathered.status.serverId || st.lastError !== '',
			);
		},
	});
}

async function deleteServer(): Promise<void> {
	let gathered: Awaited<ReturnType<typeof wipeFacts>>;
	try {
		gathered = await wipeFacts(true);
	} catch (error) {
		manageMessage.value = errorMessage(error);
		return;
	}

	confirmWipe({
		title: 'deleteServerTitle',
		text: 'deleteServerText',
		ok: 'deleteServerOk',
		danger: true,
		facts: gathered.facts,
		action: async () => {
			await serverApi.deleteServer();
			showStopped('stoppedDeletedText');
		},
	});
}
</script>
