<template>
	<section aria-labelledby="stopped-h">
		<h1 id="stopped-h" ref="heading" tabindex="-1" :class="['text-headline-small', s.title]">
			{{ t('stoppedTitle') }}
		</h1>
		<p>{{ t(text) }}</p>
	</section>
</template>

<script setup lang="ts">
import { onMounted, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import type { MessageKey } from '@/i18n';
import s from '@/styles/section.module.scss';

defineProps<{ text: MessageKey }>();

const { t } = useI18n();
const heading = useTemplateRef<HTMLElement>('heading');

// The button that stopped the server is gone with the sections: the focus
// moves to what replaced them.
onMounted(() => heading.value?.focus());
</script>
