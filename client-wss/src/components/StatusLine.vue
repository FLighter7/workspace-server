<style lang="scss" module>
.line {
	margin-block: 0.5rem;

	&:empty {
		margin: 0;
	}
}

.error {
	color: rgb(var(--v-theme-error));
}
</style>

<template>
	<!-- Always in the DOM, so a screen reader hears each message that lands in it. -->
	<p role="status" :class="[$style.line, message?.error && $style.error]">
		<MessageText v-if="message" :message="message" />
	</p>
</template>

<script setup lang="ts">
import type { Message } from '@/composables/messages';
import MessageText from './MessageText.vue';

defineProps<{ message: Message | null }>();
</script>
