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

// A device a tile: what kind it is, its name over when it paired, its state at
// the right (the text, then the dot), and Remove. A phone puts the state under
// the name.
.row {
	display: grid;
	grid-template-columns: auto minmax(0, 1fr) auto auto;
	align-items: center;
	gap: 0.25rem 0.75rem;
	padding: 0.625rem 0.5rem 0.625rem 0.75rem;
	background: rgba(var(--v-theme-on-surface), 0.04);
	border-radius: 0.5rem;

	@media (max-width: 30rem) {
		grid-template-columns: auto minmax(0, 1fr) auto;

		.kind {
			grid-row: 1 / span 2;
		}

		.state {
			grid-column: 2;
			grid-row: 2;
			justify-self: start;
		}

		.remove {
			grid-column: 3;
			grid-row: 1 / span 2;
		}
	}
}

.kind {
	display: grid;
	place-items: center;
	width: 2.5rem;
	height: 2.5rem;
	background: rgb(var(--v-theme-surface));
	border-radius: 50%;
}

.name {
	margin: 0;
	overflow-wrap: anywhere;
}

.meta {
	margin: 0;
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
		<VCard v-if="app" variant="elevated">
			<VCardTitle
				:id="titleId"
				ref="heading"
				tag="h2"
				tabindex="-1"
				:class="['text-wrap', $style.title]"
			>
				<I18nT keypath="devicesTitle" scope="global">
					<template #name>
						<bdi>{{ app.name }}</bdi>
					</template>
				</I18nT>
			</VCardTitle>

			<VCardText class="pt-0">
				<VProgressLinear v-if="devices === null" indeterminate :aria-label="t('loading')" />
				<p v-else-if="shown.length === 0" class="text-medium-emphasis">{{ t('noDevices') }}</p>
				<ul v-else :class="$style.list">
					<li v-for="device in shown" :key="device.id" :class="$style.row">
						<span :class="[$style.kind, 'text-medium-emphasis']">
							<VIcon :icon="kindIcon(device.name)" />
						</span>
						<div>
							<h3 :class="['text-title-small', $style.name]">
								<bdi>{{ device.name }}</bdi>
							</h3>
							<p :class="['text-body-small', 'text-medium-emphasis', $style.meta]">
								{{ t('pairedAt') }} {{ format.when(device.pairedAt) }}
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
						<VBtn
							:class="$style.remove"
							:icon="mdiLinkVariantOff"
							color="error"
							variant="text"
							density="comfortable"
							:disabled="removing !== null"
							:title="t('disconnect')"
							:aria-label="t('disconnectLabel', { name: device.name })"
							@click="remove(device)"
						/>
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
import { mdiCellphone, mdiLinkVariantOff, mdiMonitor, mdiTablet } from '@mdi/js';
import { serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo } from '@/api/types';
import { errorMessage, info } from '@/composables/messages';
import { ask } from '@/composables/useConfirm';
import { useFormat } from '@/composables/useFormat';
import { notify } from '@/composables/useNotify';
import { useReturnFocus } from '@/composables/useReturnFocus';

/**
 * The devices paired with one app, opened from its card's Devices tile: each
 * a tile with what kind it is (by its name), when it paired, its state at the
 * right (connected now, or when it was last seen) and Disconnect, an icon,
 * which disconnects it after the same question as on the Devices page. The list is the section's, polled with the apps, so a
 * device comes and goes here as it does there; a removed one leaves it at once.
 * Removing the last one closes the dialog: there is nothing left to list. What
 * a removal came to is said in the notification centre.
 */
const props = defineProps<{
	app: AppInfo | null;
	/** The app's devices; null until the server has listed them. */
	devices: DeviceInfo[] | null;
}>();
const open = defineModel<boolean>({ required: true });
const emit = defineEmits<{ removed: [id: string] }>();

const { t } = useI18n();
const format = useFormat();
const titleId = useId();
const heading = useTemplateRef<{ $el: HTMLElement }>('heading');
/** The device being removed. */
const removing = ref<string | null>(null);
/** What the dialog showed as it closed, kept while it fades out. */
const closing = ref<DeviceInfo[]>([]);

/** The connected ones first, then by when they were last seen, the latest first. */
function byPresence(a: DeviceInfo, b: DeviceInfo): number {
	if (a.connected !== b.connected) return a.connected ? -1 : 1;
	return Date.parse(b.lastSeen) - Date.parse(a.lastSeen);
}

const sorted = computed(() => [...(props.devices ?? [])].sort(byPresence));
const shown = computed(() => (open.value ? sorted.value : closing.value));

// Taken the moment it closes, however it closes: a list taken earlier would
// bring back a device removed since, for as long as the dialog fades out.
watch(
	open,
	(isOpen) => {
		if (!isOpen) closing.value = sorted.value;
	},
	{ flush: 'sync' },
);

useReturnFocus(open);

/**
 * What kind of device a name tells: a device names itself by its system and
 * browser when it pairs ("iPhone Safari", "Windows Chrome (PWA)").
 */
function kindIcon(name: string): string {
	if (/iPhone|Android/.test(name)) return mdiCellphone;
	if (/iPad/.test(name)) return mdiTablet;
	return mdiMonitor;
}

/** The title first, so a screen reader starts at the top. */
function focusTitle(): void {
	heading.value?.$el.focus();
}

async function remove(device: DeviceInfo): Promise<void> {
	const question = {
		title: 'disconnectTitle',
		name: device.name,
		text: 'removeText',
		ok: 'disconnect',
	} as const;
	if (!(await ask(question))) return;
	removing.value = device.id;
	try {
		await serverApi.removeDevice(device.id);
	} catch (error) {
		notify(errorMessage(error));
		return;
	} finally {
		removing.value = null;
	}
	notify(info('disconnected'));
	if (sorted.value.every((d) => d.id === device.id)) {
		// Nothing left to list; the tile it opened from is plain now, so the
		// focus goes to the section's heading. Closed before the section drops
		// the device, it fades out as it was, not saying there are no devices.
		open.value = false;
		emit('removed', device.id);
		return;
	}
	emit('removed', device.id);
	// Its row has gone with the button that had the focus.
	focusTitle();
}
</script>
