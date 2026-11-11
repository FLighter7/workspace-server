/// <reference types="vite/client" />

import type en from './i18n/locales/en.json';

type Messages = typeof en;

declare module 'vue-i18n' {
	// Every t() key is checked against the English catalog at type-check time.
	// eslint-disable-next-line @typescript-eslint/no-empty-object-type
	export interface DefineLocaleMessage extends Messages {}
}
