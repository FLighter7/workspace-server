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

.actions {
	display: flex;
	flex-wrap: wrap;
	gap: 0.5rem;
}
</style>

<template>
	<div v-if="code.qr" :class="$style.code">
		<img :class="$style.qr" :src="code.qr" :alt="t('qrAlt')" width="512" height="512" />
		<p :class="$style.timer">
			<I18nT keypath="expiresIn" scope="global">
				<template #time>
					<bdi>{{ formatLeft(code.left) }}</bdi>
				</template>
			</I18nT>
		</p>
		<div ref="field">
			<VTextarea
				:model-value="code.text"
				:label="t('codeText')"
				rows="4"
				readonly
				spellcheck="false"
			/>
		</div>
		<div :class="$style.actions">
			<VBtn @click="copy">{{ t('copy') }}</VBtn>
		</div>
	</div>
	<p v-if="code.expired">{{ t('expired') }}</p>
</template>

<script setup lang="ts">
import { useTemplateRef } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { info, type Message } from '@/composables/messages';
import { formatLeft, type PairingCodeState } from '@/composables/usePairingCode';

/** A pairing code on screen: its QR code, the time left, and its text with Copy. */
const props = defineProps<{ code: PairingCodeState }>();
/** What Copy did, for the panel's status line. */
const emit = defineEmits<{ message: [message: Message] }>();

const { t } = useI18n();
const field = useTemplateRef<HTMLElement>('field');

async function copy(): Promise<void> {
	try {
		await navigator.clipboard.writeText(props.code.text);
		emit('message', info('copied'));
	} catch {
		field.value?.querySelector('textarea')?.select();
		emit('message', info('copyFailed'));
	}
}
</script>
