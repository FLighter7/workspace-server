<template>
	<FactItem v-for="item in apps" :key="item.id">
		<template #label>
			<bdi>{{ item.name || item.id }}</bdi>
		</template>
		{{ item.size || item.modified ? format.bytes(item.size) : t('noFiles') }}
		<template v-if="open && (item.size || item.modified)">
			<br />
			<VBtn
				size="small"
				variant="text"
				class="mt-1"
				:aria-label="t('openFolderLabel', { app: item.name || item.id })"
				@click="emit('open', item)"
			>
				{{ t('openFolder') }}
			</VBtn>
		</template>
	</FactItem>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import { useFormat } from '@/composables/useFormat';
import FactItem from './FactItem.vue';
import type { AppFactItem } from './wipe';

defineProps<{
	apps: readonly AppFactItem[];
	/** An "Open folder" button for each app that has files, to copy them before they go. */
	open: boolean;
}>();
const emit = defineEmits<{ open: [app: AppFactItem] }>();

const { t } = useI18n();
const format = useFormat();
</script>
