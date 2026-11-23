<template>
	<div>
		<p class="mb-2">
			<I18nT keypath="inviteIntro" scope="global">
				<template #name>
					<bdi>{{ main }}</bdi>
				</template>
			</I18nT>
		</p>
		<p class="text-medium-emphasis mb-4">{{ t('inviteRules') }}</p>
		<PairTargetField :model-value="invitedCode.state.machine" @update:model-value="newCode" />
		<div :class="s.actions">
			<VBtn color="primary" variant="flat" @click="newCode()">{{ t('newCode') }}</VBtn>
		</div>
		<StatusLine :message="message" />

		<PairingCodeView :code="invitedCode.state" @message="message = $event" />
	</div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import PairingCodeView from '@/components/PairingCodeView.vue';
import PairTargetField from '@/components/PairTargetField.vue';
import StatusLine from '@/components/StatusLine.vue';
import type { Message } from '@/composables/messages';
import { upstreamError } from '@/composables/upstream';
import { invitedCode } from '@/composables/usePairingCode';
import s from '@/styles/section.module.scss';

/** main: the main server's name, which issues the code. */
defineProps<{ main: string }>();

const { t } = useI18n();
const message = ref<Message | null>(null);

/**
 * Asks the main for a code over the link, for machine when given (one of the
 * main's devices, chosen in the field), else for the same one; the main has
 * to be reachable.
 */
async function newCode(machine?: string): Promise<void> {
	try {
		await invitedCode.issue(machine);
	} catch (error) {
		message.value = upstreamError(error, { unreachable: 'inviteUnreachable' });
		return;
	}
	message.value = null;
}

// As on Pair a device: a code still valid is kept, so opening the dialog
// again does not revoke it.
onMounted(() => {
	if (!invitedCode.valid()) void newCode();
});
</script>
