<style lang="scss" module>
.facts {
	display: grid;
	grid-template-columns: minmax(8rem, max-content) 1fr;
	gap: 0.375rem 1rem;
	margin: 0;

	@media (max-width: 30rem) {
		grid-template-columns: 1fr;
		gap: 0 1rem;
	}

	// Each fact is a <div> around its <dt> and <dd>; the grid lays out those two.
	> div {
		display: contents;
	}

	dt {
		color: rgba(var(--v-theme-on-surface), var(--v-medium-emphasis-opacity));
	}

	dd {
		margin: 0;
		min-width: 0;
		overflow-wrap: anywhere;

		@media (max-width: 30rem) {
			margin-block-end: 0.5rem;
		}
	}
}
</style>

<template>
	<dl :class="$style.facts"><slot /></dl>
</template>
