<template>
	<FullScreenDialog v-model="open" :title="t('pairDevice')">
		<!-- Opening issues a code, and the refusals are polled only while they
		     are on screen. -->
		<PairPanel />
	</FullScreenDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import FullScreenDialog from '@/components/FullScreenDialog.vue';
import PairPanel from './PairPanel.vue';

const open = defineModel<boolean>({ required: true });

const { t } = useI18n();
</script>
