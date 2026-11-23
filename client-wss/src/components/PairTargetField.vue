<style lang="scss" module>
.field {
	max-width: 24rem;
}
</style>

<template>
	<!-- With no device paired yet there is nothing to choose: the code is for a new one. -->
	<VSelect
		v-if="machines.length"
		v-model="target"
		:class="[$style.field, 'mb-4']"
		:label="t('device')"
		:items="items"
		:hint="t('pairForHint')"
		persistent-hint
	/>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { serverApi } from '@/api/client';
import type { MachineInfo } from '@/api/types';

/**
 * Which of the user's devices a pairing code is for: a new one, or one with
 * another app paired already, which the device that pairs then joins, so the
 * Devices page shows that phone once, with all its apps. The model is the
 * machine's id, "" for a new one; a machine that is gone reads as a new one,
 * as the server takes it.
 */
const model = defineModel<string>({ required: true });

const { t } = useI18n();
const machines = ref<MachineInfo[]>([]);

onMounted(async () => {
	try {
		machines.value = await serverApi.machines();
	} catch {
		// The code is then for a new device, which can be merged later.
	}
});

const items = computed(() => [
	{ title: t('newMachine'), value: '' },
	...machines.value.map((m) => ({ title: m.name, value: m.id })),
]);

const target = computed({
	get: () => (machines.value.some((m) => m.id === model.value) ? model.value : ''),
	set: (value: string) => {
		model.value = value;
	},
});
</script>
