<style lang="scss" module>
// The name on the first line, with Open folder and Delete, two icons, centred on
// it at the right; the ID on the line under the name. A long name wraps and
// leaves the icons where they are.
.head {
	display: grid;
	grid-template:
		'name actions' auto
		'id id' auto
		/ minmax(0, 1fr) auto;
	align-items: center;
	column-gap: 0.75rem;
}

.name {
	grid-area: name;
	min-width: 0;
	margin: 0;
	overflow-wrap: anywhere;
}

.id {
	grid-area: id;
	min-width: 0;
	overflow-wrap: anywhere;
}

.actions {
	grid-area: actions;
	display: flex;
	align-items: center;
	margin-inline-end: -0.5rem;
}

// The space used (UsageBar) the card's width, and Change at the right of it.
.usage {
	display: grid;
	grid-template-columns: minmax(0, 1fr) auto;
	align-items: center;
	margin-block-start: 0.75rem;

	// The gap is Change's own, so with no Change the bar reaches the edge.
	.change {
		margin-inline-start: 1rem;
	}
}

// The last write and the devices, two tiles in a row of three columns, so each
// keeps a third of the card; a phone puts the last write on a line of its own,
// over the devices at half the width.
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
}

.tile {
	min-width: 0;
	padding: 0.5rem 0.75rem;
	background: rgba(var(--v-theme-on-surface), 0.04);
	border-radius: 0.5rem;

	dd {
		margin: 0;
		overflow-wrap: anywhere;
	}
}

// The Devices tile opens the app's devices: its number is a button whose
// ::after covers the whole tile, so a click anywhere on it opens them, while
// the label stays the <dt> of a <dl>. The chevron says it opens something.
.devices {
	position: relative;
	padding-inline-end: 2.25rem;

	&:hover {
		background: rgba(var(--v-theme-primary), 0.08);
	}

	&:has(.open:focus-visible) {
		outline: 2px solid rgb(var(--v-theme-primary));
		outline-offset: 2px;
	}
}

.open {
	padding: 0;
	font: inherit;
	color: inherit;
	text-align: start;
	cursor: pointer;
	background: none;
	border: 0;
	outline: none;

	&::after {
		content: '';
		position: absolute;
		inset: 0;
		border-radius: inherit;
	}
}

// Whole, on the number's line or, in a narrow tile, on the next.
.online {
	display: inline-block;
	margin-inline-start: 0.375rem;
	color: rgb(var(--v-theme-success));
	white-space: nowrap;
}

.chevron {
	position: absolute;
	inset-inline-end: 0.5rem;
	inset-block-start: 50%;
	translate: 0 -50%;
	pointer-events: none;
}

// What needs the user: a divergence, a sync the quota holds, a full quota.
.marks {
	display: grid;
	gap: 0.75rem;
	margin-block-start: 0.75rem;

	p {
		margin: 0.25rem 0 0;
	}
}

.note {
	margin: 0.75rem 0 0;
}

.error {
	color: rgb(var(--v-theme-error));
}
</style>

<template>
	<li>
		<!-- Flat, so the card is the colour of the header (the theme's surface): tonal always tints it. -->
		<VCard variant="flat">
			<VCardText>
				<div :class="$style.head">
					<h2 :class="['text-title-large', $style.name]">
						<bdi>{{ app.name }}</bdi>
					</h2>
					<code :class="[$style.id, 'text-medium-emphasis']">{{ app.id }}</code>
					<div :class="$style.actions">
						<VBtn
							class="text-medium-emphasis"
							:icon="mdiFolderOpenOutline"
							variant="text"
							density="comfortable"
							:loading="opening"
							:title="t('openFolderTitle', { path: app.folder })"
							:aria-label="t('openFolderPath', { app: app.name, path: app.folder })"
							@click="emit('openFolder')"
						/>
						<VBtn
							:icon="mdiDeleteOutline"
							color="error"
							variant="text"
							density="comfortable"
							:title="t('delete')"
							:aria-label="t('deleteLabel', { app: app.name })"
							@click="emit('delete')"
						/>
					</div>
				</div>

				<div :class="$style.usage">
					<UsageBar
						:size="app.size"
						:quota="app.quota"
						:label="t('quotaUsed', { app: app.name })"
					/>
					<VBtn
						v-if="!app.followsMain"
						:class="$style.change"
						variant="text"
						:aria-label="t('changeQuotaLabel', { app: app.name })"
						@click="emit('quota')"
					>
						{{ t('changeQuota') }}
					</VBtn>
				</div>

				<dl :class="$style.tiles">
					<div :class="$style.tile">
						<dt class="text-body-small text-medium-emphasis">{{ t('modified') }}</dt>
						<dd class="text-title-medium">
							{{ app.modified ? format.time(app.modified) : t('notYet') }}
						</dd>
					</div>
					<!-- With no devices there is nothing to list: a plain tile, like the last write. -->
					<div v-if="deviceCount === 0" :class="$style.tile">
						<dt class="text-body-small text-medium-emphasis">{{ t('tabDevices') }}</dt>
						<dd class="text-title-medium">{{ format.number(0) }}</dd>
					</div>
					<div v-else :class="[$style.tile, $style.devices]">
						<dt class="text-body-small text-medium-emphasis">{{ t('tabDevices') }}</dt>
						<dd class="text-title-medium">
							<button
								type="button"
								:class="$style.open"
								aria-haspopup="dialog"
								:aria-label="devicesLabel"
								@click="emit('devices')"
							>
								{{ format.number(deviceCount)
								}}<span v-if="online" :class="['text-body-small', $style.online]">{{
									t('onlineHint', { count: format.number(online) })
								}}</span>
							</button>
							<VIcon :class="[$style.chevron, 'text-medium-emphasis']" :icon="mdiChevronRight" />
						</dd>
					</div>
				</dl>

				<div v-if="hasMarks" :class="$style.marks">
					<div v-for="other in app.diverged" :key="other.serverId">
						<StateBadge tone="warning">{{ t('divergedMark') }}</StateBadge>
						<I18nT keypath="divergedText" tag="p" scope="global">
							<template #name>
								<bdi>{{ other.name || other.serverId }}</bdi>
							</template>
						</I18nT>
					</div>
					<div v-if="app.failing === 'quota'">
						<StateBadge tone="error">{{ t('failingMark') }}</StateBadge>
						<p>{{ t('failingText') }}</p>
					</div>
					<p v-if="app.size >= app.quota" :class="$style.error">{{ t('quotaFull') }}</p>
				</div>
				<I18nT
					v-if="app.followsMain"
					keypath="quotaFollowsMain"
					tag="p"
					scope="global"
					:class="[$style.note, 'text-medium-emphasis']"
				>
					<template #name>
						<bdi>{{ mainName }}</bdi>
					</template>
				</I18nT>
			</VCardText>
		</VCard>
	</li>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { mdiChevronRight, mdiDeleteOutline, mdiFolderOpenOutline } from '@mdi/js';
import type { AppInfo } from '@/api/types';
import { useFormat } from '@/composables/useFormat';
import StateBadge from './StateBadge.vue';
import UsageBar from './UsageBar.vue';

/**
 * One app of the list: the name, with Open folder and Delete, two icons, at
 * the right of it, and the ID under it; the space the files take of the quota
 * (the sizes in the unit that fits over a bar, and Change at the right); the
 * last write and the number of devices, two tiles, the second of which opens
 * the app's devices; and what needs the user (a divergence, a sync the quota
 * holds, a full quota). The folder's path is the Open folder button's name and
 * its tooltip. What to do about any of it is the parent's: the card only says
 * so and asks.
 */
const props = defineProps<{
	app: AppInfo;
	/** The name of the main server, for an app whose quota the main sets. */
	mainName: string;
	/** The app's folder is being opened. */
	opening: boolean;
	/** How many devices it has, as its Devices tile lists them. */
	deviceCount: number;
	/** How many of its devices are connected now; null while that is not known. */
	online: number | null;
}>();
const emit = defineEmits<{ delete: []; quota: []; openFolder: []; devices: [] }>();

const { t } = useI18n();
const format = useFormat();

/** The Devices tile's name: the app, the number, and how many are connected now when known. */
const devicesLabel = computed(() => {
	const values = { app: props.app.name, count: format.number(props.deviceCount) };
	return props.online === null
		? t('devicesLabel', values)
		: t('devicesOnlineLabel', { ...values, online: format.number(props.online) });
});

const hasMarks = computed(
	() =>
		props.app.diverged.length > 0 ||
		props.app.failing === 'quota' ||
		props.app.size >= props.app.quota,
);
</script>
