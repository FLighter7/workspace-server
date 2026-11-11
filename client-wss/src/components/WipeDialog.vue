<template>
	<VDialog
		v-model="open"
		max-width="560"
		:aria-labelledby="titleId"
		:aria-describedby="textId"
		@after-enter="focusCancel"
	>
		<VCard v-if="wipe" variant="elevated">
			<VCardTitle :id="titleId" tag="h2" class="text-wrap">{{ t(wipe.title) }}</VCardTitle>

			<VCardText class="pt-0">
				<p :id="textId" class="mb-4">{{ t(wipe.text) }}</p>

				<FactList>
					<template v-for="(fact, index) in wipe.facts" :key="index">
						<AppDataFacts v-if="fact.kind === 'apps'" :apps="fact.apps" :download="fact.download" />
						<FactItem v-else-if="fact.kind === 'path'" :label="t(fact.label)">
							<code>{{ fact.path }}</code>
						</FactItem>
						<FactItem v-else :label="t(fact.label)">{{ format.number(fact.value) }}</FactItem>
					</template>
				</FactList>

				<StatusLine :message="message" />
			</VCardText>

			<VCardActions>
				<VSpacer />
				<VBtn ref="cancel" variant="text" @click="open = false">{{ t('cancel') }}</VBtn>
				<VBtn
					:color="wipe.danger ? 'error' : 'primary'"
					variant="flat"
					:disabled="busy"
					@click="run"
				>
					{{ t(wipe.ok) }}
				</VBtn>
			</VCardActions>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { ref, useId, useTemplateRef, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { errorMessage, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { useReturnFocus } from '@/composables/useReturnFocus';
import AppDataFacts from './AppDataFacts.vue';
import FactItem from './FactItem.vue';
import FactList from './FactList.vue';
import StatusLine from './StatusLine.vue';
import type { Wipe } from './wipe';

const props = defineProps<{ wipe: Wipe | null }>();
const open = defineModel<boolean>({ required: true });

const { t } = useI18n();
const format = useFormat();
const titleId = useId();
const textId = useId();
const cancel = useTemplateRef<{ $el: HTMLElement }>('cancel');
const busy = ref(false);
const message = ref<Message | null>(null);

useReturnFocus(open);

watch(open, (isOpen) => {
	if (isOpen) {
		busy.value = false;
		message.value = null;
	}
});

function focusCancel(): void {
	cancel.value?.$el.focus();
}

async function run(): Promise<void> {
	if (!props.wipe) return;
	busy.value = true;
	try {
		await props.wipe.action();
	} catch (error) {
		message.value = errorMessage(error);
		busy.value = false;
		return;
	}
	open.value = false;
}
</script>
