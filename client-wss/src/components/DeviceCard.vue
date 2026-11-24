<style lang="scss" module>
// What kind of device it is, its name (and a line under it, if any), its state
// and its buttons, icons, at the right. A long name wraps and leaves them where
// they are; a phone puts the state under the name.
.head {
	display: grid;
	grid-template-columns: auto minmax(0, 1fr) auto auto;
	align-items: center;
	gap: 0.25rem 0.75rem;

	@media (max-width: 30rem) {
		grid-template-columns: auto minmax(0, 1fr) auto;

		.kind {
			grid-row: 1 / span 2;
		}

		.state {
			grid-column: 2;
			grid-row: 2;
			justify-self: start;
		}

		.actions {
			grid-column: 3;
			grid-row: 1 / span 2;
		}
	}
}

.kind {
	display: grid;
	place-items: center;
	width: 2.5rem;
	height: 2.5rem;
	background: rgba(var(--v-theme-on-surface), 0.06);
	border-radius: 50%;
}

.name {
	margin: 0;
	overflow-wrap: anywhere;
}

// Connected now in green, or when it was last seen in grey; the dot after it.
// As in the dialog of a device's apps.
.state {
	display: inline-flex;
	align-items: center;
	gap: 0.4em;
	color: rgba(var(--v-theme-on-surface), var(--v-medium-emphasis-opacity));
	white-space: nowrap;

	&::after {
		content: '';
		flex: none;
		width: 0.55em;
		height: 0.55em;
		background: currentColor;
		border-radius: 50%;
	}
}

.on {
	color: rgb(var(--v-theme-success));
}

.actions {
	display: flex;
	align-items: center;
	margin-inline-end: -0.5rem;
}

// Tiles a third of the card wide, as on an app's card; a phone puts the first
// on a line of its own.
.tiles {
	display: grid;
	grid-template-columns: repeat(3, minmax(0, 1fr));
	gap: 0.5rem;
	margin: 1rem 0 0;

	@media (max-width: 30rem) {
		grid-template-columns: repeat(2, minmax(0, 1fr));

		> :first-child {
			grid-column: 1 / -1;
		}
	}

	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
}

// Each tile is a <div> around its <dt> and <dd>, from the slot; a class of its
// own there wins over this.
:where(.tiles) > div {
	min-width: 0;
	padding: 0.5rem 0.75rem;
	background: rgba(var(--v-theme-on-surface), 0.04);
	border-radius: 0.5rem;
}
</style>

<template>
	<li>
		<!-- Flat, as an app's card: the colour of the header (the theme's surface). -->
		<VCard variant="flat">
			<VCardText>
				<div :class="$style.head">
					<span :class="[$style.kind, 'text-medium-emphasis']">
						<VIcon :icon="icon" />
					</span>
					<div>
						<component :is="headingTag" :class="['text-title-large', $style.name]">
							<bdi>{{ name }}</bdi>
						</component>
						<slot name="subtitle" />
					</div>
					<span v-if="connected" :class="['text-body-medium', $style.state, $style.on]">{{
						t('connectedNow')
					}}</span>
					<span v-else-if="away" :class="['text-body-medium', $style.state]">{{ away }}</span>
					<span
						v-else
						:class="['text-body-medium', $style.state]"
						:title="`${t('lastSeen')}: ${format.time(lastSeen)}`"
						><span class="d-sr-only">{{ t('lastSeen') }}: </span>{{ format.when(lastSeen) }}</span
					>
					<div :class="$style.actions">
						<slot name="actions" />
					</div>
				</div>

				<dl v-if="$slots.tiles" :class="$style.tiles">
					<slot name="tiles" />
				</dl>

				<slot />
			</VCardText>
		</VCard>
	</li>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import { useFormat } from '@/composables/useFormat';

/**
 * A card of the Devices page, the same for one of the user's devices and for a
 * linked server: an icon of what kind it is, its name with a line under it, if
 * any (the `subtitle` slot), its state (connected now, or when it was last
 * seen), its buttons, then tiles (the `tiles` slot, each a `<div>` of a `<dt>`
 * and a `<dd>`), if any, and whatever else it shows.
 */
withDefaults(
	defineProps<{
		name: string;
		icon: string;
		connected: boolean;
		lastSeen: string;
		/** The state while it is not connected, in place of when it was last seen. */
		away?: string;
		/** The name's heading: h3 under a heading of the section's, h2 with none. */
		headingTag?: 'h2' | 'h3';
	}>(),
	{ away: undefined, headingTag: 'h2' },
);

const { t } = useI18n();
const format = useFormat();
</script>
