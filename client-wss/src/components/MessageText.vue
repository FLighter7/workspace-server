<template>
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

<script setup lang="ts">
import { I18nT } from 'vue-i18n';
import type { Message } from '@/composables/messages';

/** A message's text in the page's language, its values isolated, then the server's detail. */
defineProps<{ message: Message }>();
</script>
