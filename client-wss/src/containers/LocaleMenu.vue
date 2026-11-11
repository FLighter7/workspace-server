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
	<VBtn
		:icon="mdiTranslate"
		variant="text"
		:aria-label="t('toggleLanguage')"
		@click="open = true"
	/>

	<VDialog v-model="open" max-width="360" :aria-labelledby="titleId">
		<VCard variant="elevated">
			<VCardTitle :id="titleId" tag="h2">{{ t('language') }}</VCardTitle>

			<VList>
				<VListItem
					v-for="code in SUPPORTED_LOCALES"
					:key="code"
					:active="locale === code"
					:aria-current="locale === code ? 'true' : undefined"
					:lang="code"
					color="primary"
					:title="LOCALE_NAMES[code]"
					:append-icon="locale === code ? mdiCheck : undefined"
					@click="select(code)"
				>
					<template #prepend>
						<img :src="LOCALE_FLAGS[code]" :class="$style.flag" alt="" />
					</template>
				</VListItem>
			</VList>
		</VCard>
	</VDialog>
</template>

<script setup lang="ts">
import { ref, useId } from 'vue';
import { useI18n } from 'vue-i18n';
import { mdiCheck, mdiTranslate } from '@mdi/js';
import { type AppLocale, LOCALE_FLAGS, LOCALE_NAMES, SUPPORTED_LOCALES } from '@/i18n';
import { setLocale } from '@/preferences';
import { useReturnFocus } from '@/composables/useReturnFocus';

const { t, locale } = useI18n();

const open = ref(false);
const titleId = useId();

useReturnFocus(open);

// The choice is applied by useAppLocale, which follows the preference itself.
function select(code: AppLocale): void {
	setLocale(code);
	open.value = false;
}
</script>
