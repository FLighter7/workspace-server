<template>
	<FullScreenDialog v-model="open" :title="t('inviteButton')">
		<!-- Opening asks the main for a code. -->
		<InvitePanel :main="main" />
	</FullScreenDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import FullScreenDialog from '@/components/FullScreenDialog.vue';
import InvitePanel from './InvitePanel.vue';

/** main: the main server's name. */
defineProps<{ main: string }>();
const open = defineModel<boolean>({ required: true });

const { t } = useI18n();
</script>
