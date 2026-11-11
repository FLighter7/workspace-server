import { readonly, ref } from 'vue';
import type { MessageKey } from '@/i18n';

/** The page's sections, by the hash that shows each: #server, #apps, #devices, #pair. */
export const SECTIONS = ['server', 'apps', 'devices', 'pair'] as const;
export type Section = (typeof SECTIONS)[number];

export const SECTION_TITLES: Record<Section, MessageKey> = {
	server: 'tabServer',
	apps: 'tabApps',
	devices: 'tabDevices',
	pair: 'tabPair',
};

/** The section a hash shows; an unknown or empty one shows the server. */
export function parseSection(hash: string): Section {
	const name = hash.replace(/^#/, '');
	return (SECTIONS as readonly string[]).includes(name) ? (name as Section) : 'server';
}

const current = ref<Section>(parseSection(location.hash));
window.addEventListener('hashchange', () => {
	current.value = parseSection(location.hash);
});

/** The section on screen, following the address's hash. */
export const section = readonly(current);

const stoppedText = ref<MessageKey | null>(null);

/** The text of the "stopped" panel, which replaces the sections; null while the server runs. */
export const stopped = readonly(stoppedText);

/** Replaces the page with the "stopped" panel. */
export function showStopped(text: MessageKey): void {
	stoppedText.value = text;
}
