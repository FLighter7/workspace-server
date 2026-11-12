<style lang="scss" module>
.wrap {
	display: grid;
	gap: 0.5rem;
	max-width: 32rem;
}

.video {
	width: 100%;
	max-width: 22.5rem;
	border-radius: 0.5rem;
}
</style>

<template>
	<div :class="$style.wrap">
		<video
			v-show="cameraOn"
			ref="video"
			:class="$style.video"
			:aria-label="t('cameraView')"
			muted
			playsinline
		/>
		<StatusLine :message="message" />
	</div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, useTemplateRef } from 'vue';
import { useI18n } from 'vue-i18n';
import type QrScanner from 'qr-scanner';
import StatusLine from '@/components/StatusLine.vue';
import { info, type Message } from '@/composables/messages';

/**
 * Reads a QR code with this computer's camera while it is mounted. It emits
 * every code it sees, again on every frame that holds one: the parent stops
 * it. qr-scanner loads only now, and its overlays stay off: they are SVG
 * with style attributes, which the page's CSP refuses.
 */
const emit = defineEmits<{ code: [value: string] }>();

const { t } = useI18n();
const video = useTemplateRef<HTMLVideoElement>('video');
const cameraOn = ref(false);
const message = ref<Message | null>(null);

let scanner: QrScanner | null = null;
let unmounted = false;

onMounted(async () => {
	try {
		const { default: Scanner } = await import('qr-scanner');
		if (unmounted || !video.value) return;
		scanner = new Scanner(video.value, (result) => emit('code', result.data), {
			maxScansPerSecond: 5,
			returnDetailedScanResult: true,
		});
		await scanner.start();
		if (unmounted) return;
		cameraOn.value = true;
		message.value = info('scanning');
	} catch {
		// No camera, no permission, or the module did not load: pasting
		// still works.
		scanner?.destroy();
		scanner = null;
		message.value = info('cameraFailed');
	}
});

onBeforeUnmount(() => {
	unmounted = true;
	scanner?.destroy();
	scanner = null;
});
</script>
