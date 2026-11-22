// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { serverApi } from '@/api/client';
import type { AppInfo } from '@/api/types';
import { mountPage, settle } from '@/testing/mountPage';
import DeleteAppDialog from '../DeleteAppDialog.vue';

const app: AppInfo = {
	id: 'notes',
	name: 'Notes',
	quota: 52_428_800,
	files: 3,
	size: 2048,
	modified: '2026-11-10T12:00:00Z',
	devices: 1,
	folder: '/data/WSS-Data/apps/notes',
	diverged: [],
	followsMain: false,
};

afterEach(() => {
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

function open() {
	return mountPage(DeleteAppDialog, {
		modelValue: true,
		app,
		devices: [
			{
				id: 'd1',
				app: 'notes',
				name: 'Phone',
				pairedAt: '2026-11-01T00:00:00Z',
				lastSeen: '2026-11-10T00:00:00Z',
				connected: false,
			},
			{
				id: 'd2',
				app: 'notes',
				name: 'Laptop',
				pairedAt: '2026-11-02T00:00:00Z',
				lastSeen: '2026-11-10T00:00:00Z',
				connected: false,
			},
		],
	});
}

const confirmButton = (): HTMLButtonElement =>
	[...document.querySelectorAll('button[type="submit"]')].find(
		(b) => b.textContent?.trim() === 'Delete',
	) as HTMLButtonElement;

const input = (): HTMLInputElement => document.querySelector('input') as HTMLInputElement;

describe('DeleteAppDialog', () => {
	it('shows what goes, offers to open the folder first, and asks for the id', async () => {
		const openFolder = vi.spyOn(serverApi, 'openFolder').mockResolvedValue(null);
		open();
		await settle();
		const text = document.body.textContent ?? '';
		expect(text).toContain('Delete Notes?');
		expect(text).toContain(
			'The app and all its data will be deleted, and its devices disconnected.',
		);
		expect(text).toContain('To be deleted2 kB');
		expect(text).toContain('To be disconnectedPhone, Laptop');
		expect(text).not.toContain('Files');
		expect(text).toContain("To keep it, open the app's folder first");
		expect(text).toContain('To confirm, type notes');
		expect(document.querySelector('a[download]')).toBeNull();

		(document.querySelector('button[aria-label="Open folder of Notes"]') as HTMLElement).click();
		await settle();
		expect(openFolder).toHaveBeenCalledWith('notes');
		// The window is the answer: nothing is said.
		expect(document.body.textContent).not.toContain('opens in the file manager');
	});

	it('deletes only once the typed id matches', async () => {
		const deleteApp = vi.spyOn(serverApi, 'deleteApp').mockResolvedValue(null);
		const wrapper = open();
		await settle();
		expect(confirmButton().disabled).toBe(true);

		input().value = 'note';
		input().dispatchEvent(new Event('input'));
		await settle();
		expect(confirmButton().disabled).toBe(true);

		input().value = 'notes';
		input().dispatchEvent(new Event('input'));
		await settle();
		expect(confirmButton().disabled).toBe(false);

		confirmButton().click();
		await settle();
		expect(deleteApp).toHaveBeenCalledWith('notes', 'notes');
		expect(wrapper.emitted('deleted')).toHaveLength(1);
		expect(wrapper.emitted('update:modelValue')).toEqual([[false]]);
	});

	it('stays open with the error when the server refuses', async () => {
		const { ApiError } = await import('@/api/client');
		vi.spyOn(serverApi, 'deleteApp').mockRejectedValue(new ApiError('notFound', 'app not found'));
		const wrapper = open();
		await settle();
		input().value = 'notes';
		input().dispatchEvent(new Event('input'));
		await settle();
		confirmButton().click();
		await settle();

		expect(document.body.textContent).toContain(
			'Not found: it may have been removed already. (app not found)',
		);
		expect(wrapper.emitted('deleted')).toBeUndefined();
		expect(confirmButton().disabled).toBe(false);
	});
});
