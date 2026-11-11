<template>
	<VDialog
		:model-value="confirmState.open"
		max-width="460"
		:aria-labelledby="titleId"
		:aria-describedby="textId"
		@update:model-value="(open: boolean) => open || settle(false)"
		@after-enter="focusCancel"
	>
		<VCard v-if="question" variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">
				<I18nT v-if="question.name !== undefined" :keypath="question.title" scope="global">
					<template #name>
						<bdi>{{ question.name }}</bdi>
					</template>
				</I18nT>
				<template v-else>{{ t(question.title) }}</template>
			</VCardTitle>

			<VCardText :id="textId" class="pt-0">{{ t(question.text) }}</VCardText>

			<VCardActions>
				<VSpacer />
				<VBtn ref="cancel" variant="text" @click="settle(false)">{{ t('cancel') }}</VBtn>
				<VBtn color="error" variant="flat" @click="settle(true)">{{ t(question.ok) }}</VBtn>
			</VCardActions>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { computed, toRef, useId, useTemplateRef } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { confirmState, settle } from '@/composables/useConfirm';
import { useReturnFocus } from '@/composables/useReturnFocus';

const { t } = useI18n();
const titleId = useId();
const textId = useId();
const question = computed(() => confirmState.question);
const cancel = useTemplateRef<{ $el: HTMLElement }>('cancel');

useReturnFocus(toRef(confirmState, 'open'));

// The safe answer has the focus first: Enter on an opened question cancels.
function focusCancel(): void {
	cancel.value?.$el.focus();
}
</script>
