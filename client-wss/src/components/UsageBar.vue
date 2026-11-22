<style lang="scss" module>
.usage {
	display: grid;
	row-gap: 0.125rem;
}

// Space used at the start, the sizes at the end; on a narrow line the sizes go under it.
.caption {
	display: flex;
	flex-wrap: wrap;
	justify-content: space-between;
	gap: 0 1rem;
}
</style>

<template>
	<div :class="$style.usage">
		<div :class="[$style.caption, 'text-body-small', 'text-medium-emphasis']">
			<span>{{ t('used') }}</span>
			<span>{{ usedText }}</span>
		</div>
		<VProgressLinear
			:model-value="usedPercent"
			:color="usageColor"
			height="6"
			rounded
			:aria-label="label"
			:aria-valuetext="usedText"
		/>
	</div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useFormat } from '@/composables/useFormat';

/**
 * The space an app's files take of its quota: "Space used" and the sizes, in
 * the unit that fits, over a bar. The bar warns from 90 %, and is red once
 * the files fill the quota. On the app's card and in its quota dialog.
 */
const props = defineProps<{
	/** The bytes the files take. */
	size: number;
	/** The quota, in bytes. */
	quota: number;
	/** The bar's name for a screen reader. */
	label: string;
}>();

const { t } = useI18n();
const format = useFormat();

const usedText = computed(() =>
	t('usedOf', { used: format.bytes(props.size), quota: format.bytes(props.quota) }),
);

const usedPercent = computed(() =>
	props.quota > 0 ? Math.min(100, Math.round((props.size / props.quota) * 100)) : 0,
);

const usageColor = computed(() => {
	if (props.size >= props.quota) return 'error';
	if (props.size >= props.quota * 0.9) return 'warning';
	return 'primary';
});
</script>
