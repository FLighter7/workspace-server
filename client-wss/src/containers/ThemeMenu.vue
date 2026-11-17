<template>
	<VMenu location="bottom end">
		<template #activator="{ props: activator }">
			<VBtn v-bind="activator" :icon="themeIcon" variant="text" :aria-label="t('toggleTheme')" />
		</template>

		<VList role="menu" :aria-label="t('theme')" min-width="200">
			<VListItem
				v-for="option in options"
				:key="option.value"
				:active="preferences.theme === option.value"
				role="menuitemradio"
				:aria-checked="preferences.theme === option.value"
				color="primary"
				:prepend-icon="option.icon"
				:title="t(option.label)"
				:append-icon="preferences.theme === option.value ? mdiCheck : undefined"
				@click="setTheme(option.value)"
			/>
		</VList>
	</VMenu>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiCheck, mdiThemeLightDark, mdiWeatherNight, mdiWeatherSunny } from '@mdi/js';
import type { MessageKey } from '@/i18n';
import { preferences, setTheme, type ThemePreference } from '@/preferences';

const { t } = useI18n();

const options: Array<{ value: ThemePreference; label: MessageKey; icon: string }> = [
	{ value: 'system', label: 'themeSystem', icon: mdiThemeLightDark },
	{ value: 'light', label: 'themeLight', icon: mdiWeatherSunny },
	{ value: 'dark', label: 'themeDark', icon: mdiWeatherNight },
];

const themeIcon = computed(
	() => options.find((option) => option.value === preferences.theme)?.icon ?? mdiThemeLightDark,
);

// The choice is applied by useAppTheme, which follows the preference itself.
// The menu closes by itself when an item is clicked, and hands the focus back to its button.
</script>
