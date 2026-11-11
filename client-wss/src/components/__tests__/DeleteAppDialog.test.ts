// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { serverApi } from '@/api/client';
import type { AppInfo } from '@/api/types';
import { mountPage, settle } from '@/testing/mountPage';
import DeleteAppDialog from '../DeleteAppDialog.vue';

const app: AppInfo = {
	id: 'notes',
	name: 'Notes',
	history: 10,
	maxBytes: 1024,
	size: 2048,
	modified: '2026-11-10T12:00:00Z',
	versions: 3,
	devices: 1,
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
		],
	});
}

const confirmButton = (): HTMLButtonElement =>
	[...document.querySelectorAll('button')].find((b) =>
		b.textContent?.includes('Delete for good'),
	) as HTMLButtonElement;

const input = (): HTMLInputElement => document.querySelector('input') as HTMLInputElement;

describe('DeleteAppDialog', () => {
	it('shows what goes, with the download, and asks for the id', async () => {
		open();
		await settle();
		const text = document.body.textContent ?? '';
		expect(text).toContain('Delete Notes?');
		expect(text).toContain('Phone');
		expect(document.querySelector('a[download]')?.getAttribute('href')).toBe(
			'/api/apps/notes/document',
		);
		expect(text).toContain('To confirm, type notes');
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
