import { flushPromises, mount } from '@vue/test-utils';
import { i18n } from '@/i18n';
import vuetify from '@/plugins/vuetify';

/** Mounts a component with the page's plugins, in English. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- any component, any props
export function mountPage(component: any, props: Record<string, unknown> = {}) {
	i18n.global.locale.value = 'en';
	return mount(component, {
		props,
		attachTo: document.body,
		global: { plugins: [vuetify, i18n] },
	});
}

/** Lets fetches, re-renders and dialog transitions settle. */
export async function settle(): Promise<void> {
	await flushPromises();
	await new Promise((resolve) => setTimeout(resolve, 0));
	await flushPromises();
}
