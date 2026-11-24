<style lang="scss" module>
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
	<DeviceCard
		:name="name"
		:icon="icon"
		:connected="online > 0"
		:last-seen="lastSeen"
		:heading-tag="headingTag"
	>
		<template #actions>
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
		</template>
		<template #tiles>
			<div>
				<dt class="text-body-small text-medium-emphasis">{{ t('pairedAt') }}</dt>
				<dd class="text-title-medium">{{ format.time(pairedAt) }}</dd>
			</div>
			<div :class="$style.apps">
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
		</template>
	</DeviceCard>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiChevronRight, mdiDeleteOutline, mdiPencilOutline } from '@mdi/js';
import type { DeviceInfo } from '@/api/types';
import { deviceKindIcon } from '@/composables/deviceKind';
import { useFormat } from '@/composables/useFormat';
import DeviceCard from './DeviceCard.vue';

/**
 * One of the user's devices, a phone or a laptop, with the apps paired on it:
 * what kind it is (by the name its first app paired under), its name, its
 * state (connected now, when an app on it is, or when one last was), Rename
 * and Delete (with every app on it), two icons, then two tiles: when the first
 * of its apps was paired, and the number of its apps, which opens them. What to
 * do about any of it is the parent's: the card only asks.
 */
const props = withDefaults(
	defineProps<{
		name: string;
		/** Its apps' pairings, at least one. */
		devices: DeviceInfo[];
		/** The name's heading: h3 under a Devices heading, h2 with none. */
		headingTag?: 'h2' | 'h3';
	}>(),
	{ headingTag: 'h2' },
);
const emit = defineEmits<{ apps: []; rename: []; delete: [] }>();

const { t } = useI18n();
const format = useFormat();

const icon = computed(() => deviceKindIcon(props.devices[0]?.name ?? ''));
const online = computed(() => props.devices.filter((d) => d.connected).length);
/** When the first of its apps was paired. */
const pairedAt = computed(() =>
	props.devices.reduce(
		(earliest, d) => (Date.parse(d.pairedAt) < Date.parse(earliest) ? d.pairedAt : earliest),
		props.devices[0]?.pairedAt ?? '',
	),
);
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
