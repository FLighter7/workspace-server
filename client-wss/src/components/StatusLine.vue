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
		<template v-if="message">
			<I18nT :keypath="message.key" scope="global">
				<template v-for="(value, name) in message.values" :key="name" #[name]>
					<bdi>{{ value }}</bdi>
				</template>
				<template v-for="(href, name) in message.links" :key="name" #[name]>
					<a :href="href">{{ href }}</a>
				</template>
			</I18nT>
			<template v-if="message.detail"> ({{ message.detail }})</template>
		</template>
	</p>
</template>

<script setup lang="ts">
import { I18nT } from 'vue-i18n';
import type { Message } from '@/composables/messages';

defineProps<{ message: Message | null }>();
</script>
