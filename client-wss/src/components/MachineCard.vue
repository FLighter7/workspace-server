<style lang="scss" module>
// What kind of device it is, its name, and Rename and Delete, two icons,
// at the right of it. A long name wraps and leaves the icons where they are.
.head {
	display: grid;
	grid-template-columns: auto minmax(0, 1fr) auto;
	align-items: center;
	column-gap: 0.75rem;
}

.kind {
	display: grid;
	place-items: center;
	width: 2.5rem;
	height: 2.5rem;
	background: rgba(var(--v-theme-on-surface), 0.06);
	border-radius: 50%;
}

.name {
	min-width: 0;
	margin: 0;
	overflow-wrap: anywhere;
}

.actions {
	display: flex;
	align-items: center;
	margin-inline-end: -0.5rem;
}

// When it was last seen and its apps, two tiles in a row of three columns, as
// on an app's card; a phone puts the first on a line of its own.
.tiles {
	display: grid;
	grid-template-columns: repeat(3, minmax(0, 1fr));
	gap: 0.5rem;
	margin: 1rem 0 0;

	@media (max-width: 30rem) {
		grid-template-columns: repeat(2, minmax(0, 1fr));

		> :first-child {
			grid-column: 1 / -1;
		}
	}
}

.tile {
	min-width: 0;
	padding: 0.5rem 0.75rem;
	background: rgba(var(--v-theme-on-surface), 0.04);
	border-radius: 0.5rem;

	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
}

.on {
	color: rgb(var(--v-theme-success));
}

// The Apps tile opens the device's apps: its number is a button whose ::after
// covers the whole tile, so a click anywhere on it opens them, while the label
// stays the <dt> of a <dl>. The chevron says it opens something.
.apps {
	position: relative;
	padding-inline-end: 2.25rem;

	&:hover {
		background: rgba(var(--v-theme-primary), 0.08);
	}

	&:has(.open:focus-visible) {
		outline: 2px solid rgb(var(--v-theme-primary));
		outline-offset: 2px;
	}
}

.open {
	padding: 0;
	font: inherit;
	color: inherit;
	text-align: start;
	cursor: pointer;
	background: none;
	border: 0;
	outline: none;

	&::after {
		content: '';
		position: absolute;
		inset: 0;
		border-radius: inherit;
	}
}

// Whole, on the number's line or, in a narrow tile, on the next.
.online {
	display: inline-block;
	margin-inline-start: 0.375rem;
	color: rgb(var(--v-theme-success));
	white-space: nowrap;
}

.chevron {
	position: absolute;
	inset-inline-end: 0.5rem;
	inset-block-start: 50%;
	translate: 0 -50%;
	pointer-events: none;
}
</style>

<template>
	<li>
		<!-- Flat, as an app's card: the colour of the header (the theme's surface). -->
		<VCard variant="flat">
			<VCardText>
				<div :class="$style.head">
					<span :class="[$style.kind, 'text-medium-emphasis']">
						<VIcon :icon="icon" />
					</span>
					<h3 :class="['text-title-large', $style.name]">
						<bdi>{{ name }}</bdi>
					</h3>
					<div :class="$style.actions">
						<VBtn
							class="text-medium-emphasis"
							:icon="mdiPencilOutline"
							variant="text"
							density="comfortable"
							:title="t('rename')"
							:aria-label="t('renameLabel', { name })"
							@click="emit('rename')"
						/>
						<VBtn
							:icon="mdiDeleteOutline"
							color="error"
							variant="text"
							density="comfortable"
							:title="t('delete')"
							:aria-label="t('deleteMachineLabel', { name })"
							@click="emit('delete')"
						/>
					</div>
				</div>

				<dl :class="$style.tiles">
					<div :class="$style.tile">
						<dt class="text-body-small text-medium-emphasis">{{ t('lastSeen') }}</dt>
						<dd v-if="online > 0" :class="['text-title-medium', $style.on]">
							{{ t('connectedNow') }}
						</dd>
						<dd v-else class="text-title-medium">{{ format.time(lastSeen) }}</dd>
					</div>
					<div :class="[$style.tile, $style.apps]">
						<dt class="text-body-small text-medium-emphasis">{{ t('tabApps') }}</dt>
						<dd class="text-title-medium">
							<button
								type="button"
								:class="$style.open"
								aria-haspopup="dialog"
								:aria-label="appsLabel"
								@click="emit('apps')"
							>
								{{ format.number(devices.length)
								}}<span v-if="online" :class="['text-body-small', $style.online]">{{
									t('onlineHint', { count: format.number(online) })
								}}</span>
							</button>
							<VIcon :class="[$style.chevron, 'text-medium-emphasis']" :icon="mdiChevronRight" />
						</dd>
					</div>
				</dl>
			</VCardText>
		</VCard>
	</li>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiChevronRight, mdiDeleteOutline, mdiPencilOutline } from '@mdi/js';
import type { DeviceInfo } from '@/api/types';
import { deviceKindIcon } from '@/composables/deviceKind';
import { useFormat } from '@/composables/useFormat';

/**
 * One of the user's devices, a phone or a laptop, with the apps paired on it:
 * what kind it is (by the name its first app paired under), its name (an h3,
 * under the section's Devices), with Rename and Delete (with every app on
 * it), two icons, at the right; when it
 * was last seen (connected now, when an app on it is), and the number of its
 * apps, a tile which opens them. What to do about any of it is the parent's:
 * the card only asks.
 */
const props = defineProps<{
	name: string;
	/** Its apps' pairings, at least one. */
	devices: DeviceInfo[];
}>();
const emit = defineEmits<{ apps: []; rename: []; delete: [] }>();

const { t } = useI18n();
const format = useFormat();

const icon = computed(() => deviceKindIcon(props.devices[0]?.name ?? ''));
const online = computed(() => props.devices.filter((d) => d.connected).length);
/** The latest any of its apps was seen. */
const lastSeen = computed(() =>
	props.devices.reduce(
		(latest, d) => (Date.parse(d.lastSeen) > Date.parse(latest) ? d.lastSeen : latest),
		props.devices[0]?.lastSeen ?? '',
	),
);

/** The Apps tile's name: the device, the number, and how many are connected now. */
const appsLabel = computed(() => {
	const values = { name: props.name, count: format.number(props.devices.length) };
	return online.value > 0
		? t('machineAppsOnlineLabel', { ...values, online: format.number(online.value) })
		: t('machineAppsLabel', values);
});
</script>
