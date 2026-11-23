<style lang="scss" module>
.title {
	outline: none;
}

.list {
	display: grid;
	gap: 0.5rem;
	margin: 0;
	padding: 0;
	list-style: none;
}

// An app a tile: its name over the pairing's own name and when it paired, its
// state at the right (the text, then the dot), and Move and Disconnect. A
// phone puts the state under the name.
.row {
	display: grid;
	grid-template-columns: minmax(0, 1fr) auto auto;
	align-items: center;
	gap: 0.25rem 0.75rem;
	padding: 0.625rem 0.5rem 0.625rem 0.75rem;
	background: rgba(var(--v-theme-on-surface), 0.04);
	border-radius: 0.5rem;

	@media (max-width: 30rem) {
		grid-template-columns: minmax(0, 1fr) auto;

		.state {
			grid-column: 1;
			grid-row: 2;
			justify-self: start;
		}

		.buttons {
			grid-column: 2;
			grid-row: 1 / span 2;
		}
	}
}

.name {
	margin: 0;
	overflow-wrap: anywhere;
}

.meta {
	margin: 0;
	overflow-wrap: anywhere;
}

.buttons {
	display: flex;
	align-items: center;
}

// Connected now in green, or when it was last seen in grey; the dot after it.
.state {
	display: inline-flex;
	align-items: center;
	gap: 0.4em;
	color: rgba(var(--v-theme-on-surface), var(--v-medium-emphasis-opacity));
	white-space: nowrap;

	&::after {
		content: '';
		flex: none;
		width: 0.55em;
		height: 0.55em;
		background: currentColor;
		border-radius: 50%;
	}
}

.on {
	color: rgb(var(--v-theme-success));
}
</style>

<template>
	<VDialog
		v-model="open"
		max-width="560"
		scrollable
		:aria-labelledby="titleId"
		@after-enter="focusTitle"
	>
		<VCard v-if="machine" variant="elevated">
			<VCardTitle
				:id="titleId"
				ref="heading"
				tag="h2"
				tabindex="-1"
				:class="['text-wrap', $style.title]"
			>
				<I18nT keypath="machineAppsTitle" scope="global">
					<template #name>
						<bdi>{{ machine.name }}</bdi>
					</template>
				</I18nT>
			</VCardTitle>

			<VCardText class="pt-0">
				<ul :class="$style.list">
					<li v-for="device in shown" :key="device.id" :class="$style.row">
						<div>
							<h3 :class="['text-title-small', $style.name]">
								<bdi>{{ appName(device.app) }}</bdi>
							</h3>
							<p :class="['text-body-small', 'text-medium-emphasis', $style.meta]">
								<bdi>{{ device.name }}</bdi> · {{ t('pairedAt') }}
								{{ format.when(device.pairedAt) }}
							</p>
						</div>
						<span v-if="device.connected" :class="['text-body-medium', $style.state, $style.on]">{{
							t('connectedNow')
						}}</span>
						<span
							v-else
							:class="['text-body-medium', $style.state]"
							:title="`${t('lastSeen')}: ${format.time(device.lastSeen)}`"
							><span class="d-sr-only">{{ t('lastSeen') }}: </span
							>{{ format.when(device.lastSeen) }}</span
						>
						<div :class="$style.buttons">
							<VMenu v-if="targets.length">
								<template #activator="{ props: menu }">
									<VBtn
										v-bind="menu"
										class="text-medium-emphasis"
										:icon="mdiSwapHorizontal"
										variant="text"
										density="comfortable"
										:disabled="busy !== null"
										:title="t('moveLabel', { name: appName(device.app) })"
										:aria-label="t('moveLabel', { name: appName(device.app) })"
									/>
								</template>
								<VList density="compact">
									<VListItem
										v-for="target in targets"
										:key="target.id"
										@click="move(device, target.id)"
									>
										<VListItemTitle>
											<bdi>{{ target.name }}</bdi>
										</VListItemTitle>
									</VListItem>
								</VList>
							</VMenu>
							<VBtn
								:icon="mdiLinkVariantOff"
								color="error"
								variant="text"
								density="comfortable"
								:disabled="busy !== null"
								:title="t('disconnect')"
								:aria-label="t('disconnectLabel', { name: appName(device.app) })"
								@click="remove(device)"
							/>
						</div>
					</li>
				</ul>
			</VCardText>

			<VCardActions>
				<VSpacer />
				<VBtn variant="text" @click="open = false">{{ t('close') }}</VBtn>
			</VCardActions>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { computed, ref, useId, useTemplateRef, watch } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { mdiLinkVariantOff, mdiSwapHorizontal } from '@mdi/js';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo, MachineInfo } from '@/api/types';
import { errorMessage, info } from '@/composables/messages';
import { ask } from '@/composables/useConfirm';
import { useFormat } from '@/composables/useFormat';
import { notify } from '@/composables/useNotify';
import { useReturnFocus } from '@/composables/useReturnFocus';

/**
 * The apps paired on one of the user's devices, opened from its card's Apps
 * tile: each a tile with the app's name, the name the pairing gave itself
 * (its browser) and when it paired, its state at the right (connected now, or
 * when it was last seen), Move, which puts it on another device or on one of
 * its own (two pairings taken for one device, or one taken for another), and
 * Disconnect, after the same question as anywhere. The list is the
 * section's, polled, so a pairing comes and goes here as it does there. A
 * pairing that leaves the device last closes the dialog: the device has gone
 * with it. What a change came to is said in the notification centre.
 */
const props = defineProps<{
	machine: MachineInfo | null;
	/** Its apps' pairings. */
	devices: DeviceInfo[];
	/** Every device of the user's, this one too, to move a pairing to. */
	machines: MachineInfo[];
	/** The apps, for their names. */
	apps: AppInfo[];
}>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ changed: [] }>();

const { t } = useI18n();
const format = useFormat();
const titleId = useId();
const heading = useTemplateRef<{ $el: HTMLElement }>('heading');
/** The pairing being moved or removed. */
const busy = ref<string | null>(null);
/** What the dialog showed as it closed, kept while it fades out. */
const closing = ref<DeviceInfo[]>([]);

/** The connected ones first, then by when they were last seen, the latest first. */
function byPresence(a: DeviceInfo, b: DeviceInfo): number {
	if (a.connected !== b.connected) return a.connected ? -1 : 1;
	return Date.parse(b.lastSeen) - Date.parse(a.lastSeen);
}

const sorted = computed(() => [...props.devices].sort(byPresence));
const shown = computed(() => (open.value ? sorted.value : closing.value));

watch(
	open,
	(isOpen) => {
		if (!isOpen) closing.value = sorted.value;
	},
	{ flush: 'sync' },
);

useReturnFocus(open);

function appName(id: string): string {
	return props.apps.find((app) => app.id === id)?.name ?? id;
}

/**
 * Where a pairing can go: every other device, and one of its own unless it is
 * alone on this one already.
 */
const targets = computed<MachineInfo[]>(() => {
	const others = props.machines.filter((m) => m.id !== props.machine?.id);
	return props.devices.length > 1 ? [...others, { id: '', name: t('moveToNew') }] : others;
});

/** The title first, so a screen reader starts at the top. */
function focusTitle(): void {
	heading.value?.$el.focus();
}

/** After a pairing left: the dialog closes with the last, else the focus goes to the title. */
function left(device: DeviceInfo): void {
	if (sorted.value.every((d) => d.id === device.id)) {
		open.value = false;
	} else {
		focusTitle();
	}
	emit('changed');
}

async function move(device: DeviceInfo, machine: string): Promise<void> {
	busy.value = device.id;
	try {
		await serverApi.moveDevice(device.id, machine);
	} catch (error) {
		notify(errorMessage(error));
		return;
	} finally {
		busy.value = null;
	}
	notify(info('moved'));
	left(device);
}

async function remove(device: DeviceInfo): Promise<void> {
	const name = appName(device.app);
	const question = {
		title: 'disconnectTitle',
		name,
		text: 'removeText',
		ok: 'disconnect',
	} as const;
	if (!(await ask(question))) return;
	busy.value = device.id;
	try {
		await serverApi.removeDevice(device.id);
	} catch (error) {
		notify(errorMessage(error));
		return;
	} finally {
		busy.value = null;
	}
	notify(info('disconnected'));
	left(device);
}
</script>
