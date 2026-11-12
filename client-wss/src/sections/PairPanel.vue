<template>
	<div ref="panel">
		<p class="mb-2">{{ t('pairIntro') }}</p>
		<p class="text-medium-emphasis mb-4">{{ t('pairRules') }}</p>
		<div :class="s.actions">
			<VBtn color="primary" variant="flat" @click="newCode">{{ t('newCode') }}</VBtn>
		</div>
		<StatusLine :message="message" />

		<PairingCodeView :code="ownCode.state" @message="message = $event" />

		<div v-if="refusals.length">
			<h3 :class="['text-title-large', s.subtitle]">{{ t('refusedTitle') }}</h3>
			<ul :class="s.cards">
				<ItemCard
					v-for="refusal in refusals"
					:key="`${refusal.at}|${refusal.app}|${refusal.device}`"
				>
					<p class="mb-3">
						<I18nT keypath="refused" scope="global">
							<template #app>
								<code
									><bdi>{{ refusal.app }}</bdi></code
								>
							</template>
						</I18nT>
					</p>
					<FactList>
						<FactItem :label="t('device')">
							<bdi v-if="refusal.device">{{ refusal.device }}</bdi>
							<template v-else>—</template>
						</FactItem>
						<FactItem :label="t('when')">{{ format.time(refusal.at) }}</FactItem>
					</FactList>
					<!-- Only an id the server could serve gets the offer. -->
					<template v-if="ID_PATTERN.test(refusal.app)" #actions>
						<VBtn color="primary" variant="flat" @click="addRefused(refusal.app)">
							{{ t('addIt') }}
						</VBtn>
					</template>
				</ItemCard>
			</ul>
		</div>
	</div>
</template>

<script setup lang="ts">
import { onMounted, ref, useTemplateRef } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { serverApi } from '@/api/client';
import { ID_PATTERN } from '@/api/ids';
import type { Refusal } from '@/api/types';
import FactItem from '@/components/FactItem.vue';
import FactList from '@/components/FactList.vue';
import ItemCard from '@/components/ItemCard.vue';
import PairingCodeView from '@/components/PairingCodeView.vue';
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { ownCode } from '@/composables/usePairingCode';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';

const { t } = useI18n();
const format = useFormat();
const message = ref<Message | null>(null);
const refusals = ref<Refusal[]>([]);
const panel = useTemplateRef<HTMLElement>('panel');

async function newCode(): Promise<void> {
	try {
		await ownCode.issue();
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	message.value = null;
}

// Opening the dialog issues a code unless the one on screen is still valid,
// so opening it again does not revoke it. An expired code is replaced only on
// opening again or by the New code button, never by itself.
onMounted(() => {
	if (!ownCode.valid()) void newCode();
});

async function loadRefusals(): Promise<void> {
	try {
		refusals.value = await serverApi.refusals();
	} catch {
		// The pairing itself still works; the next poll retries.
	}
}

usePolling(loadRefusals);

async function addRefused(id: string): Promise<void> {
	try {
		await serverApi.addApp({ id });
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	message.value = info('addedRetry');
	await loadRefusals();
	// The panel lives in a dialog: the focus stays there, on the dialog's title.
	await rescueFocus(panel.value?.closest('[role="dialog"]')?.querySelector<HTMLElement>('h2'));
}
</script>
