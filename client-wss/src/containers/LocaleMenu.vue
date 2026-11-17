<style lang="scss" module>
.flag {
	width: 1.5rem;
	height: 1.125rem;
	margin-inline-end: 1rem;
	object-fit: cover;
	border-radius: 2px;
}
</style>

<template>
	<VMenu location="bottom end">
		<template #activator="{ props: activator }">
			<VBtn
				v-bind="activator"
				:icon="mdiTranslate"
				variant="text"
				:aria-label="t('toggleLanguage')"
			/>
		</template>

		<VList role="menu" :aria-label="t('language')" min-width="200">
			<VListItem
				v-for="code in SUPPORTED_LOCALES"
				:key="code"
				:active="locale === code"
				role="menuitemradio"
				:aria-checked="locale === code"
				:lang="code"
				color="primary"
				:title="LOCALE_NAMES[code]"
				:append-icon="locale === code ? mdiCheck : undefined"
				@click="setLocale(code)"
			>
				<template #prepend>
					<img :src="LOCALE_FLAGS[code]" :class="$style.flag" alt="" />
				</template>
			</VListItem>
		</VList>
	</VMenu>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import { mdiCheck, mdiTranslate } from '@mdi/js';
import { LOCALE_FLAGS, LOCALE_NAMES, SUPPORTED_LOCALES } from '@/i18n';
import { setLocale } from '@/preferences';

const { t, locale } = useI18n();

// The choice is applied by useAppLocale, which follows the preference itself.
// The menu closes by itself when an item is clicked, and hands the focus back to its button.
</script>
