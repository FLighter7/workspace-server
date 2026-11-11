<template>
	<FactItem v-for="item in apps" :key="item.id">
		<template #label>
			<bdi>{{ item.name || item.id }}</bdi>
		</template>
		{{ item.size || item.modified ? format.bytes(item.size) : t('noDocument') }} ·
		{{ t('versions') }}: {{ format.number(item.versions) }}
		<template v-if="download && item.modified">
			<br />
			<a :href="documentUrl(item.id)" download>{{ t('download') }}</a>
		</template>
	</FactItem>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import { documentUrl } from '@/api/client';
import { useFormat } from '@/composables/useFormat';
import FactItem from './FactItem.vue';
import type { AppFactItem } from './wipe';

defineProps<{
	apps: readonly AppFactItem[];
	/** A download link for each app that has a document. */
	download: boolean;
}>();

const { t } = useI18n();
const format = useFormat();
</script>
