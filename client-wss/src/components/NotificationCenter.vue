<template>
	<!-- One snackbar for the whole page, at the bottom right: green for what was done, red for
		an error, which stays longer and has Close. Hovering or focusing it holds it. -->
	<VSnackbar
		:model-value="noticeState.open"
		:color="notice?.error ? 'error' : 'success'"
		:timeout="notice?.error ? 10000 : 4000"
		location="bottom end"
		@update:model-value="(open: boolean) => open || dismiss()"
		@after-leave="showNext"
	>
		<MessageText v-if="notice" :message="notice" />
		<template v-if="notice?.error" #actions>
			<VBtn @click="dismiss">{{ t('close') }}</VBtn>
		</template>
	</VSnackbar>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { dismiss, noticeState, showNext } from '@/composables/useNotify';
import MessageText from './MessageText.vue';

const { t } = useI18n();
const notice = computed(() => noticeState.current);
</script>
