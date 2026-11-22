<style lang="scss" module>
.top,
.row {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: 0.5rem 1rem;
}

.id {
	min-width: 0;
	overflow-wrap: anywhere;
}

.name {
	min-width: 0;
	margin: 0;
	overflow-wrap: anywhere;
}

// The bar takes half the card's width, the text follows it, and Change sits at
// the right edge, under Delete. A phone leaves no room for all three on a line:
// there the bar takes a line of its own, and the text and Change sit under it.
.usage {
	display: flex;
	align-items: center;
	gap: 0.25rem 0.75rem;

	.bar {
		flex: 0 1 50%;
		width: auto;
	}

	.used {
		min-width: 0;
	}

	.change {
		flex: none;
		margin-inline-start: auto;
	}

	@media (max-width: 30rem) {
		flex-wrap: wrap;

		.bar {
			flex: 1 0 100%;
		}
	}
}

.details {
	margin-block-start: 0.75rem;
}

// The folder is a button that reads as a link: it opens the folder in the
// file manager, and a long path wraps instead of cutting off.
.folder {
	padding: 0;
	font: inherit;
	color: rgb(var(--v-theme-primary));
	text-align: start;
	text-decoration: underline;
	overflow-wrap: anywhere;
	cursor: pointer;
	background: none;
	border: 0;

	&:disabled {
		cursor: progress;
		opacity: var(--v-disabled-opacity);
	}

	&:focus-visible {
		outline: 2px solid currentcolor;
		outline-offset: 2px;
	}
}

// What needs the user: a divergence, a sync the quota holds, a full quota.
// It stays in view while the card is folded.
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
				<div :class="$style.top">
					<code :class="[$style.id, 'text-medium-emphasis']">{{ app.id }}</code>
					<VBtn
						:icon="expanded ? mdiChevronUp : mdiChevronDown"
						size="small"
						variant="text"
						:aria-label="t(expanded ? 'hideDetails' : 'showDetails', { app: app.name })"
						:aria-expanded="expanded"
						:aria-controls="detailsId"
						@click="expanded = !expanded"
					/>
				</div>

				<div :class="$style.row">
					<h2 :class="['text-title-medium', $style.name]">
						<bdi>{{ app.name }}</bdi>
					</h2>
					<VBtn color="error" variant="text" @click="emit('delete')">{{ t('delete') }}</VBtn>
				</div>

				<div :class="$style.usage">
					<VProgressLinear
						:class="$style.bar"
						:model-value="usedPercent"
						:color="usageColor"
						height="6"
						rounded
						:aria-label="t('quotaUsed', { app: app.name })"
						:aria-valuetext="usedText"
					/>
					<span :class="$style.used">{{ usedText }}</span>
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

				<VExpandTransition>
					<div v-show="expanded" :id="detailsId" :class="$style.details">
						<FactList>
							<FactItem :label="t('folder')">
								<button
									type="button"
									:class="$style.folder"
									:disabled="opening"
									:aria-label="t('openFolderPath', { app: app.name, path: app.folder })"
									@click="emit('openFolder')"
								>
									<code>{{ app.folder }}</code>
								</button>
							</FactItem>
							<FactItem :label="t('modified')">
								{{ app.modified ? format.time(app.modified) : t('notYet') }}
							</FactItem>
							<FactItem :label="t('files')">
								{{ app.files > 0 ? format.number(app.files) : t('noFiles') }}
							</FactItem>
							<FactItem :label="t('tabDevices')">{{ format.number(app.devices) }}</FactItem>
						</FactList>
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
					</div>
				</VExpandTransition>
			</VCardText>
		</VCard>
	</li>
</template>

<script setup lang="ts">
import { computed, ref, useId } from 'vue';
import { I18nT, useI18n } from 'vue-i18n';
import { mdiChevronDown, mdiChevronUp } from '@mdi/js';
import type { AppInfo } from '@/api/types';
import { useFormat } from '@/composables/useFormat';
import FactItem from './FactItem.vue';
import FactList from './FactList.vue';
import StateBadge from './StateBadge.vue';

/**
 * One app of the list. Folded, it shows three lines: the ID; the name, with
 * Delete; the space the files take of the quota (a bar, the sizes in the unit
 * that fits, and Change at the right, under Delete). Unfolded, it adds the data folder (a button that
 * opens it), the last write, the number of files and of devices. What needs
 * the user (a divergence, a sync the quota holds, a full quota) shows folded
 * as well; what to do about it is the parent's: the card only says so.
 */
const props = defineProps<{
	app: AppInfo;
	/** The name of the main server, for an app whose quota the main sets. */
	mainName: string;
	/** The app's folder is being opened. */
	opening: boolean;
}>();
const emit = defineEmits<{ delete: []; quota: []; openFolder: [] }>();

const { t } = useI18n();
const format = useFormat();
const detailsId = useId();
/** Folded when the card appears; the list is polled, and the card keeps its state. */
const expanded = ref(false);

const usedText = computed(() =>
	t('usedOf', { used: format.bytes(props.app.size), quota: format.bytes(props.app.quota) }),
);

const usedPercent = computed(() =>
	props.app.quota > 0 ? Math.min(100, Math.round((props.app.size / props.app.quota) * 100)) : 0,
);

/** The bar warns from 90 %, and is red once the files fill the quota. */
const usageColor = computed(() => {
	if (props.app.size >= props.app.quota) return 'error';
	if (props.app.size >= props.app.quota * 0.9) return 'warning';
	return 'primary';
});

const hasMarks = computed(
	() =>
		props.app.diverged.length > 0 ||
		props.app.failing === 'quota' ||
		props.app.size >= props.app.quota,
);
</script>
