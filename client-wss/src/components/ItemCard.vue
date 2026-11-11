<style lang="scss" module>
.head {
	display: flex;
	flex-wrap: wrap;
	align-items: center;
	justify-content: space-between;
	gap: 0.5rem 1rem;
	margin-block-end: 0.75rem;
}

.title {
	min-width: 0;
	margin: 0;
	overflow-wrap: anywhere;
}

.actions {
	display: flex;
	flex-wrap: wrap;
	gap: 0.5rem;
	margin-block-start: 0.75rem;
}
</style>

<template>
	<li>
		<VCard>
			<VCardText>
				<div v-if="$slots.title || $slots.side" :class="$style.head">
					<component :is="headingTag" :class="['text-title-medium', $style.title]">
						<slot name="title" />
					</component>
					<slot name="side" />
				</div>
				<slot />
				<div v-if="$slots.actions" :class="$style.actions">
					<slot name="actions" />
				</div>
			</VCardText>
		</VCard>
	</li>
</template>

<script setup lang="ts">
withDefaults(defineProps<{ headingTag?: 'h2' | 'h3' }>(), { headingTag: 'h2' });
</script>
