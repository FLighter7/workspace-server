<style lang="scss" module>
.code {
	display: grid;
	gap: 1rem;
	max-width: 32rem;
	margin-block: 1rem;
}

// The QR code keeps its own white background in the dark theme: phones scan
// dark modules on light best.
.qr {
	width: 100%;
	max-width: 20rem;
	height: auto;
	image-rendering: pixelated;
	border: 1px solid rgb(var(--v-theme-outline));
	border-radius: 0.5rem;
}

.timer {
	margin: 0;
	font-variant-numeric: tabular-nums;
}
</style>

<template>
	<section aria-labelledby="pair-h">
		<h1 id="pair-h" tabindex="-1" :class="['text-headline-small', s.title]">{{ t('tabPair') }}</h1>
		<p class="mb-2">{{ t('pairIntro') }}</p>
		<p class="text-medium-emphasis mb-4">{{ t('pairRules') }}</p>
		<div :class="s.actions">
			<VBtn color="primary" variant="flat" @click="newCode">{{ t('newCode') }}</VBtn>
		</div>
		<StatusLine :message="message" />

		<div v-if="pairingCode.qr" :class="$style.code">
			<img :class="$style.qr" :src="pairingCode.qr" :alt="t('qrAlt')" width="512" height="512" />
			<p :class="$style.timer">
				<I18nT keypath="expiresIn" scope="global">
					<template #time>
						<bdi>{{ formatLeft(pairingCode.left) }}</bdi>
					</template>
				</I18nT>
			</p>
			<div ref="codeField">
				<VTextarea
					:model-value="pairingCode.text"
					:label="t('codeText')"
					rows="4"
					readonly
					spellcheck="false"
				/>
			</div>
			<div :class="s.actions">
				<VBtn @click="copy">{{ t('copy') }}</VBtn>
			</div>
		</div>
		<p v-if="pairingCode.expired">{{ t('expired') }}</p>

		<div v-if="refusals.length">
			<h2 :class="['text-title-large', s.subtitle]">{{ t('refusedTitle') }}</h2>
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
	</section>
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
import StatusLine from '@/components/StatusLine.vue';
import { errorMessage, info, type Message } from '@/composables/messages';
import { useFormat } from '@/composables/useFormat';
import { codeValid, formatLeft, issueCode, pairingCode } from '@/composables/usePairingCode';
import { usePolling } from '@/composables/usePolling';
import { rescueFocus } from '@/composables/useReturnFocus';
import s from '@/styles/section.module.scss';

const { t } = useI18n();
const format = useFormat();
const message = ref<Message | null>(null);
const refusals = ref<Refusal[]>([]);
const codeField = useTemplateRef<HTMLElement>('codeField');

async function newCode(): Promise<void> {
	try {
		await issueCode();
	} catch (error) {
		message.value = errorMessage(error);
		return;
	}
	message.value = null;
}

// Entering the section issues a code unless the one on screen is still valid,
// so coming back does not revoke it. An expired code is replaced only on
// entering again or by the New code button, never by itself.
onMounted(() => {
	if (!codeValid()) void newCode();
});

async function copy(): Promise<void> {
	try {
		await navigator.clipboard.writeText(pairingCode.text);
		message.value = info('copied');
	} catch {
		codeField.value?.querySelector('textarea')?.select();
		message.value = info('copyFailed');
	}
}

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
	await rescueFocus();
}
</script>
