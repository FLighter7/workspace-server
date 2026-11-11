<template>
	<VBtn :icon="themeIcon" variant="text" :aria-label="t('toggleTheme')" @click="open = true" />

	<VDialog v-model="open" max-width="360" :aria-labelledby="titleId">
		<VCard variant="elevated">
			<VCardTitle :id="titleId" tag="h2">{{ t('theme') }}</VCardTitle>

			<VList>
				<VListItem
					v-for="option in options"
					:key="option.value"
					:active="preferences.theme === option.value"
					:aria-current="preferences.theme === option.value ? 'true' : undefined"
					color="primary"
					:prepend-icon="option.icon"
					:title="t(option.label)"
					:append-icon="preferences.theme === option.value ? mdiCheck : undefined"
					@click="select(option.value)"
				/>
			</VList>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { computed, ref, useId } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiCheck, mdiThemeLightDark, mdiWeatherNight, mdiWeatherSunny } from '@mdi/js';
import type { MessageKey } from '@/i18n';
import { preferences, setTheme, type ThemePreference } from '@/preferences';
import { useReturnFocus } from '@/composables/useReturnFocus';

const { t } = useI18n();

const open = ref(false);
const titleId = useId();

useReturnFocus(open);

const options: Array<{ value: ThemePreference; label: MessageKey; icon: string }> = [
	{ value: 'system', label: 'themeSystem', icon: mdiThemeLightDark },
	{ value: 'light', label: 'themeLight', icon: mdiWeatherSunny },
	{ value: 'dark', label: 'themeDark', icon: mdiWeatherNight },
];

const themeIcon = computed(
	() => options.find((option) => option.value === preferences.theme)?.icon ?? mdiThemeLightDark,
);

// The choice is applied by useAppTheme, which follows the preference itself.
function select(value: ThemePreference): void {
	setTheme(value);
	open.value = false;
}
</script>
