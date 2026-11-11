<style lang="scss" module>
.brand {
	margin: 0;
	padding-inline-start: 1rem;
	font-weight: 600;
	white-space: nowrap;
	overflow: hidden;
	text-overflow: ellipsis;
}

// The sections are links, not tabs: each has its own address (#server, …).
.nav {
	display: flex;
	width: 100%;
	height: 100%;
	overflow-x: auto;
	scrollbar-width: none;
}

.tab {
	flex: none;

	// Centred while they fit; auto margins, unlike justify-content, still let
	// the first link scroll into view when they do not.
	&:first-child {
		margin-inline-start: auto;
	}

	&:last-child {
		margin-inline-end: auto;
	}
	height: 100% !important;
	text-transform: none;
	letter-spacing: normal;
}

.current {
	color: rgb(var(--v-theme-primary));
	box-shadow: inset 0 -2px 0 rgb(var(--v-theme-primary));
}

.main {
	max-width: 48rem;
	margin-inline: auto;
	padding: 1.5rem 1rem 3rem;
}
</style>

<template>
	<VApp>
		<VAppBar border="b">
			<p :class="$style.brand">Workspace Server</p>
			<VSpacer />
			<template #append>
				<ThemeMenu />
				<LocaleMenu />
			</template>

			<template v-if="!stopped" #extension>
				<nav ref="nav" :class="$style.nav" :aria-label="t('sections')">
					<VBtn
						v-for="name in SECTIONS"
						:key="name"
						:href="`#${name}`"
						:class="[$style.tab, section === name && $style.current]"
						:aria-current="section === name ? 'page' : undefined"
						variant="text"
						rounded="0"
					>
						{{ t(SECTION_TITLES[name]) }}
					</VBtn>
				</nav>
			</template>
		</VAppBar>

		<VMain>
			<div :class="$style.main">
				<StoppedPanel v-if="stopped" :text="stopped" />
				<ServerSection v-else-if="section === 'server'" />
				<DevicesSection v-else-if="section === 'devices'" />
				<AppsSection v-else />
			</div>
		</VMain>

		<ConfirmDialog />
	</VApp>
</template>

<script setup lang="ts">
import { nextTick, useTemplateRef, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import ConfirmDialog from '@/components/ConfirmDialog.vue';
import LocaleMenu from '@/containers/LocaleMenu.vue';
import ThemeMenu from '@/containers/ThemeMenu.vue';
import { useAppLocale, useAppTheme } from '@/composables/useAppearance';
import { SECTIONS, SECTION_TITLES, section, stopped } from '@/composables/useSection';
import AppsSection from '@/sections/AppsSection.vue';
import DevicesSection from '@/sections/DevicesSection.vue';
import ServerSection from '@/sections/ServerSection.vue';
import StoppedPanel from '@/sections/StoppedPanel.vue';

const { t } = useI18n();

useAppTheme();
useAppLocale();

// On a phone the links scroll sideways: keep the current one in view.
const nav = useTemplateRef<HTMLElement>('nav');
watch(
	section,
	async () => {
		await nextTick();
		nav.value
			?.querySelector('[aria-current]')
			?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
	},
	{ immediate: true },
);
</script>
