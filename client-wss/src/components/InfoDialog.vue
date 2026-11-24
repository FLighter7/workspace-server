<template>
	<VDialog
		v-model="open"
		max-width="460"
		:aria-labelledby="titleId"
		:aria-describedby="textId"
		@after-enter="focusClose"
	>
		<VCard variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">{{ title }}</VCardTitle>

			<VCardText :id="textId" class="px-4 pt-0">{{ text }}</VCardText>

			<VCardActions>
				<VSpacer />
				<VBtn ref="close" variant="text" @click="open = false">{{ t('close') }}</VBtn>
			</VCardActions>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { useId, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import { useReturnFocus } from '@/composables/useReturnFocus';

/** What something on the page is, behind an information icon by its title. */
defineProps<{ title: string; text: string }>();
const open = defineModel<boolean>({ required: true });

const { t } = useI18n();
const titleId = useId();
const textId = useId();
const close = useTemplateRef<{ $el: HTMLElement }>('close');

useReturnFocus(open);

function focusClose(): void {
	close.value?.$el.focus();
}
</script>
