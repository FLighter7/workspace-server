<style lang="scss" module>
// Under a line; the buttons' own height is room enough below.
.foot {
	padding-block-start: 0.5rem;
	margin-block: 1rem -0.5rem;
	border-block-start: thin solid rgba(var(--v-border-color), var(--v-border-opacity));
}

// Details and "Make it the main" at the two edges of the card: plain buttons,
// whose text alone changes colour, since a background would stick out of those
// edges.
.buttons {
	display: flex;
	flex-wrap: wrap;
	justify-content: space-between;
	gap: 0.25rem 1rem;
}

.text {
	padding: 0;
	transition: color 0.2s;

	// Plain fades its text until it is hovered, under the contrast it needs.
	&:not(:disabled) {
		opacity: 1;
	}

	// Plain has no overlay to show the focus with.
	&:focus-visible {
		outline: 2px solid rgb(var(--v-theme-primary));
		outline-offset: 2px;
	}
}

.details {
	color: rgba(var(--v-theme-on-surface), var(--v-medium-emphasis-opacity));

	&:hover {
		color: rgb(var(--v-theme-on-surface));
	}
}

.main {
	margin-inline-start: auto;
	color: rgb(var(--v-theme-primary));

	&:hover {
		color: color-mix(in srgb, rgb(var(--v-theme-primary)) 50%, rgb(var(--v-theme-on-surface)));
	}
}

.chevron {
	transition: rotate 0.2s;

	@media (prefers-reduced-motion: reduce) {
		transition: none;
	}
}

.open {
	rotate: 180deg;
}

.facts {
	padding-block: 0.5rem;
}
</style>

<template>
	<DeviceCard
		:name="device.name"
		:icon="mdiDesktopClassic"
		:connected="device.connected"
		:last-seen="device.lastSeen"
		:away="server ? undefined : t('serverNotAnnounced')"
		heading-tag="h3"
	>
		<template #subtitle>
			<p class="text-body-medium text-medium-emphasis ma-0">
				{{ t('pairedAt') }} {{ format.time(device.pairedAt) }}
			</p>
		</template>
		<template #actions>
			<VBtn
				v-if="server"
				class="text-medium-emphasis"
				:icon="mdiPencilOutline"
				variant="text"
				density="comfortable"
				:title="t('rename')"
				:aria-label="t('renameLabel', { name: device.name })"
				@click="emit('rename')"
			/>
			<VBtn
				:icon="mdiDeleteOutline"
				color="error"
				variant="text"
				density="comfortable"
				:title="t('delete')"
				:aria-label="t('deleteLabel', { app: device.name })"
				@click="emit('delete')"
			/>
		</template>

		<div v-if="server" :class="$style.foot">
			<div :class="$style.buttons">
				<VBtn
					:class="[$style.text, $style.details]"
					variant="plain"
					:ripple="false"
					:aria-label="t('detailsLabel', { name: device.name })"
					:aria-expanded="open"
					:aria-controls="detailsId"
					@click="open = !open"
				>
					{{ t('details') }}
					<template #append>
						<VIcon :class="[$style.chevron, { [$style.open]: open }]" :icon="mdiChevronDown" />
					</template>
				</VBtn>
				<VBtn
					:class="[$style.text, $style.main]"
					variant="plain"
					:ripple="false"
					:disabled="!device.connected"
					:loading="handingOver"
					@click="emit('handOver')"
				>
					{{ t('handOverButton') }}
				</VBtn>
			</div>
			<VExpandTransition>
				<div v-show="open" :id="detailsId">
					<FactList :class="$style.facts">
						<FactItem :label="t('appId')">
							<code>{{ server.serverId }}</code>
						</FactItem>
						<FactItem :label="t('addresses')">
							<ul :class="s.plain">
								<li v-for="host in server.hosts" :key="host">
									<code>{{ host }}</code>
								</li>
							</ul>
						</FactItem>
						<FactItem :label="t('udpPort')">{{ server.port }}</FactItem>
					</FactList>
				</div>
			</VExpandTransition>
		</div>
	</DeviceCard>
</template>

<script setup lang="ts">
import { computed, ref, useId } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiChevronDown, mdiDeleteOutline, mdiDesktopClassic, mdiPencilOutline } from '@mdi/js';
import type { DeviceInfo } from '@/api/types';
import { useFormat } from '@/composables/useFormat';
import s from '@/styles/section.module.scss';
import DeviceCard from './DeviceCard.vue';
import FactItem from './FactItem.vue';
import FactList from './FactList.vue';

/**
 * A server linked to this one, a card like a device's: its name with when it
 * was paired under it, its state, Rename and Delete, two icons, and under a
 * line Details, which unfolds its ID and where it said it is reached, and
 * "Make it the main", only while it is connected. One that has not announced
 * itself yet has nothing to unfold, and can be neither renamed nor made the
 * main. What to do about any of it is the parent's: the card only asks.
 */
const props = defineProps<{
	device: DeviceInfo;
	/** The role of main is being handed to it. */
	handingOver: boolean;
}>();
const emit = defineEmits<{ rename: []; delete: []; handOver: [] }>();

const { t } = useI18n();
const format = useFormat();
const detailsId = useId();
const open = ref(false);

const server = computed(() => props.device.server);
</script>
