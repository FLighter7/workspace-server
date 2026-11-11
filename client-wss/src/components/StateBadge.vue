<style lang="scss" module>
.state {
	display: inline-flex;
	align-items: baseline;
	gap: 0.4em;

	&::before {
		content: '';
		flex: none;
		width: 0.55em;
		height: 0.55em;
		border-radius: 50%;
		background: currentColor;
	}
}

.on {
	color: rgb(var(--v-theme-success));
}

.off {
	color: rgba(var(--v-theme-on-surface), var(--v-medium-emphasis-opacity));
}

.error {
	color: rgb(var(--v-theme-error));
}
</style>

<template>
	<span :class="[$style.state, $style[tone]]"><slot /></span>
</template>

<script setup lang="ts">
defineProps<{ tone: 'on' | 'off' | 'error' }>();
</script>
