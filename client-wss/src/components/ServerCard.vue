<style lang="scss" module>
.facts,
.action {
	margin-block-start: 1rem;
}
</style>

<template>
	<DeviceCard
		:id="device.server?.serverId"
		:name="name"
		:icon="mdiDesktopClassic"
		:connected="device.connected"
		:last-seen="device.lastSeen"
		heading-tag="h3"
	>
		<template #actions>
			<VBtn
				:icon="mdiDeleteOutline"
				color="error"
				variant="text"
				density="comfortable"
				:title="t('delete')"
				:aria-label="t('deleteLabel', { app: name })"
				@click="emit('delete')"
			/>
		</template>
		<template #tiles>
			<div>
				<dt class="text-body-small text-medium-emphasis">{{ t('pairedAt') }}</dt>
				<dd class="text-title-medium">{{ format.time(device.pairedAt) }}</dd>
			</div>
		</template>

		<FactList :class="$style.facts">
			<template v-if="device.server">
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
		</FactList>
		<VBtn
			v-if="device.server"
			:class="$style.action"
			:loading="handingOver"
			@click="emit('handOver')"
		>
			{{ t('handOverButton') }}
		</VBtn>
	</DeviceCard>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiDeleteOutline, mdiDesktopClassic } from '@mdi/js';
import type { DeviceInfo } from '@/api/types';
import { useFormat } from '@/composables/useFormat';
import s from '@/styles/section.module.scss';
import DeviceCard from './DeviceCard.vue';
import FactItem from './FactItem.vue';
import FactList from './FactList.vue';

/**
 * A server linked to this one, a card like a device's: its name (the one it
 * announced, its pairing's until it has) with its ID under it, its state,
 * Delete, when it was paired, then where it said it is reached, and "Make it
 * the main" once it has. What to do about any of it is the parent's: the card
 * only asks.
 */
const props = defineProps<{
	device: DeviceInfo;
	/** The role of main is being handed to it. */
	handingOver: boolean;
}>();
const emit = defineEmits<{ delete: []; handOver: [] }>();

const { t } = useI18n();
const format = useFormat();

const name = computed(() => props.device.server?.name ?? props.device.name);
</script>
