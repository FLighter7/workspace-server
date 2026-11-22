// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { serverApi } from '@/api/client';
import type { AppInfo, Status } from '@/api/types';
import { digitsOnly } from '@/composables/digitsOnly';
import { MB } from '@/composables/useFormat';
import { mountPage, settle } from '@/testing/mountPage';
import QuotaDialog from '../QuotaDialog.vue';

const app: AppInfo = {
	id: 'notes',
	name: 'Notes',
	quota: 50 * MB,
	files: 3,
	// 30 MB and a bit: the quota can go no lower than 31 MB.
	size: 30 * MB + 1,
	modified: '2026-11-10T12:00:00Z',
	devices: 0,
	folder: '/data/WSS-Data/apps/notes',
	diverged: [],
	followsMain: false,
};

afterEach(() => {
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

const submit = () => document.querySelector('form')?.dispatchEvent(new Event('submit'));

describe('QuotaDialog', () => {
	it('shows the space used, and takes no quota under it', async () => {
		const setQuota = vi.spyOn(serverApi, 'setQuota').mockResolvedValue(null);
		vi.spyOn(serverApi, 'status').mockResolvedValue({ dataFree: 1000 * MB } as Status);
		const wrapper = mountPage(QuotaDialog, { modelValue: false, app });
		await wrapper.setProps({ modelValue: true });
		await settle();
		const text = document.body.textContent ?? '';
		expect(text).toContain('Data of Notes');
		expect(text).toContain('30 MB of 50 MB');
		expect(text).toContain('It cannot be less than the space already taken.');
		// The top is the free space with what the files take already.
		expect(text).toContain('From 31 to 1,030 MB');
		expect(
			document
				.querySelector('[role="progressbar"][aria-hidden="false"]')
				?.getAttribute('aria-label'),
		).toBe('Space used of the quota of Notes');

		const field = document.querySelector('input') as HTMLInputElement;
		expect(field.type).toBe('text');
		field.value = '30';
		field.dispatchEvent(new Event('input'));
		await settle();
		submit();
		await settle();
		expect(setQuota).not.toHaveBeenCalled();
		expect(document.body.textContent).toContain('Enter a whole number from 31 to 1,030.');

		field.value = '31';
		field.dispatchEvent(new Event('input'));
		await settle();
		submit();
		await settle();
		expect(setQuota).toHaveBeenCalledWith('notes', 31 * MB);
	});

	it('closes on Save with the field as it opened, though the files have outgrown it', async () => {
		const setQuota = vi.spyOn(serverApi, 'setQuota').mockResolvedValue(null);
		vi.spyOn(serverApi, 'status').mockResolvedValue({ dataFree: 1000 * MB } as Status);
		const wrapper = mountPage(QuotaDialog, {
			modelValue: false,
			app: { ...app, quota: 20 * MB },
		});
		await wrapper.setProps({ modelValue: true });
		await settle();
		submit();
		await settle();
		expect(setQuota).not.toHaveBeenCalled();
		expect(wrapper.emitted('update:modelValue')).toEqual([[false]]);
	});
});

describe('digitsOnly', () => {
	function field(value: string, at: number): HTMLInputElement {
		const input = document.createElement('input');
		document.body.append(input);
		input.value = value;
		input.setSelectionRange(at, at);
		input.addEventListener('beforeinput', (event) => digitsOnly(event as InputEvent));
		return input;
	}

	it('lets digits through', () => {
		const input = field('1', 1);
		const event = new InputEvent('beforeinput', { data: '2', cancelable: true });
		input.dispatchEvent(event);
		expect(event.defaultPrevented).toBe(false);
	});

	it('refuses a letter or a sign', () => {
		for (const data of ['a', '-', ',', 'e']) {
			const input = field('1', 1);
			const event = new InputEvent('beforeinput', { data, cancelable: true });
			input.dispatchEvent(event);
			expect(event.defaultPrevented).toBe(true);
			expect(input.value).toBe('1');
		}
	});

	it('keeps the digits of a paste, where the caret is', () => {
		const input = field('15', 1);
		const typed = vi.fn();
		input.addEventListener('input', typed);
		const event = new InputEvent('beforeinput', {
			data: '2 0',
			inputType: 'insertFromPaste',
			cancelable: true,
		});
		input.dispatchEvent(event);
		expect(event.defaultPrevented).toBe(true);
		expect(input.value).toBe('1205');
		expect(typed).toHaveBeenCalled();
	});
});
