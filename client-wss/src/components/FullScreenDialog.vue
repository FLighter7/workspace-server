<style lang="scss" module>
.title {
	margin: 0;
	padding-inline-start: 1rem;
	white-space: nowrap;
	overflow: hidden;
	text-overflow: ellipsis;
}

.body {
	width: 100%;
	max-width: 48rem;
	margin-inline: auto;
	padding: 1.5rem 1rem 3rem;
}
</style>

<template>
	<VDialog v-model="open" fullscreen :aria-labelledby="titleId" @after-enter="focusTitle">
		<VCard variant="flat" rounded="0">
			<VToolbar color="surface" border="b">
				<h2 :id="titleId" ref="heading" tabindex="-1" :class="['text-title-large', $style.title]">
					{{ title }}
				</h2>
				<VSpacer />
				<VBtn :icon="mdiClose" variant="text" :aria-label="t('close')" @click="open = false" />
			</VToolbar>

			<div :class="$style.body">
				<!-- Mounted while open only: what is inside issues and polls only
				     while it is on screen. -->
				<slot v-if="open" />
			</div>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { mdiClose } from '@mdi/js';
import { useId, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import { useReturnFocus } from '@/composables/useReturnFocus';

/** A page-sized dialog with its title and a Close button. */
defineProps<{ title: string }>();
const open = defineModel<boolean>({ required: true });

const { t } = useI18n();
const titleId = useId();
const heading = useTemplateRef<HTMLElement>('heading');

useReturnFocus(open);

/** The title first, so a screen reader starts at the top of a page-sized dialog. */
function focusTitle(): void {
	heading.value?.focus();
}
</script>
