// @vitest-environment happy-dom
import { afterEach, describe, expect, it } from 'vitest';
import { dismiss, noticeState, notify, resetNotices, showNext } from '@/composables/useNotify';
import { mountPage, settle } from '@/testing/mountPage';
import NotificationCenter from '../NotificationCenter.vue';

const snackbar = (): HTMLElement | null => document.querySelector('.v-snackbar');

afterEach(() => {
	resetNotices();
	document.body.innerHTML = '';
});

describe('NotificationCenter', () => {
	it('says what was done in green, with no button', async () => {
		mountPage(NotificationCenter);
		notify({ key: 'quotaSaved', values: { app: 'Todo', quota: '30 MB' } });
		await settle();
		expect(snackbar()?.textContent).toContain('The quota of Todo is now 30 MB.');
		expect(snackbar()?.querySelector('.bg-success')).not.toBeNull();
		expect(snackbar()?.querySelector('button')).toBeNull();
	});

	it("says an error in red, with the server's detail and Close", async () => {
		mountPage(NotificationCenter);
		notify({ key: 'errNotFound', detail: 'no such app', error: true });
		await settle();
		expect(snackbar()?.textContent).toContain('Not found: it may have been removed already.');
		expect(snackbar()?.textContent).toContain('(no such app)');
		expect(snackbar()?.querySelector('.bg-error')).not.toBeNull();
		const close = snackbar()?.querySelector('button');
		expect(close?.textContent?.trim()).toBe('Close');
		close?.click();
		expect(noticeState.open).toBe(false);
	});

	it('shows one at a time, the next once the one on screen has gone', () => {
		notify({ key: 'added' });
		notify({ key: 'deleted' });
		expect(noticeState.current?.key).toBe('added');
		dismiss();
		showNext();
		expect(noticeState.current?.key).toBe('deleted');
		expect(noticeState.open).toBe(true);
		dismiss();
		showNext();
		expect(noticeState.current).toBeNull();
		expect(noticeState.open).toBe(false);
	});
});
