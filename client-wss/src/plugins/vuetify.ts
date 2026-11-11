import 'vuetify/styles';
import '@/styles/page.scss';
import { createVuetify } from 'vuetify';
import { aliases, mdi } from 'vuetify/iconsets/mdi-svg';
import { en, ru, pl } from 'vuetify/locale';
import { boot } from '@/boot';
import { initialLocale } from '@/i18n';
import { preferences } from '@/preferences';
import { THEMES } from './theme';

export default createVuetify({
	theme: {
		// The page's CSP lets in only the stylesheet that carries this nonce.
		cspNonce: boot.nonce || undefined,
		// 'system' follows the OS's light or dark, live; useAppTheme keeps the
		// choice in step from here on.
		defaultTheme: preferences.theme,
		themes: THEMES,
	},
	defaults: {
		VAppBar: { elevation: 0, color: 'surface' },
		VBtn: { variant: 'tonal' },
		VCard: { variant: 'outlined' },
		// Messages render only once there are some: an empty area under a field is
		// still a role="alert", announced each time a dialog opens.
		VTextField: { variant: 'outlined', hideDetails: 'auto' },
		VTextarea: { variant: 'outlined', hideDetails: 'auto' },
	},
	icons: {
		defaultSet: 'mdi',
		aliases,
		sets: { mdi },
	},
	// Vuetify's own strings (close buttons and the like) stay in its locale
	// system: vue-i18n's adapter would need the message compiler, which the
	// precompiled messages drop.
	locale: {
		locale: initialLocale,
		fallback: 'en',
		messages: { en, ru, pl },
	},
});
