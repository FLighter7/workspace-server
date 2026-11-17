// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, serverApi } from '@/api/client';
import type { AppInfo, UpstreamStatus } from '@/api/types';
import { mountPage, settle } from '@/testing/mountPage';
import AppsSection from '../AppsSection.vue';

const MB = 1024 * 1024;

const todo: AppInfo = {
	id: 'todo',
	name: 'Todo',
	quota: 50 * MB,
	files: 3,
	size: 5 * MB,
	modified: '2026-11-13T10:00:00Z',
	devices: 2,
	folder: 'C:\\ws\\WSS-Data\\apps\\todo',
	diverged: [],
	followsMain: false,
};

const notes: AppInfo = {
	id: 'notes',
	name: 'Notes',
	quota: 2 * MB,
	files: 0,
	size: 0,
	modified: null,
	devices: 0,
	folder: 'C:\\ws\\WSS-Data\\apps\\notes',
	diverged: [],
	followsMain: false,
};

const asMain: UpstreamStatus = {
	linked: false,
	connected: false,
	halted: false,
	apps: [],
	subordinates: 1,
	term: 0,
};

const asSubordinate: UpstreamStatus = {
	linked: true,
	serverId: 'MainServer01',
	name: 'Desk',
	connected: true,
	halted: false,
	apps: ['todo'],
	subordinates: 0,
	term: 0,
};

let apps: ReturnType<typeof vi.spyOn>;
let upstream: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
	apps = vi.spyOn(serverApi, 'apps').mockResolvedValue([todo, notes]);
	upstream = vi.spyOn(serverApi, 'upstream').mockResolvedValue(asMain);
	vi.spyOn(serverApi, 'status').mockResolvedValue({ dataFree: null } as never);
});

afterEach(() => {
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

const text = (): string => document.body.textContent?.replace(/\s+/g, ' ') ?? '';

const byLabel = (label: string): HTMLButtonElement | null =>
	document.querySelector(`button[aria-label="${label}"]`);

/** The card of the app whose heading names it. */
const card = (name: string): HTMLElement =>
	[...document.querySelectorAll('li')].find((li) =>
		li.querySelector('h2')?.textContent?.includes(name),
	) as HTMLElement;

const dialog = (): HTMLElement => document.querySelector('[role="dialog"]') as HTMLElement;

/** The button at the top of the page, which opens the form for a new app. */
async function openAddDialog(): Promise<void> {
	byLabel('Add an app')?.click();
	await settle();
}

describe('AppsSection on a main', () => {
	it("shows each app's files, the space they take of the quota, its folder and the totals", async () => {
		mountPage(AppsSection);
		await settle();

		const first = card('Todo').textContent ?? '';
		expect(first).toContain('3');
		expect(first).toContain('5 MB of 50 MB');
		expect(first).toContain('C:\\ws\\WSS-Data\\apps\\todo');
		const bar = card('Todo').querySelector('[role="progressbar"][aria-hidden="false"]');
		expect(bar?.getAttribute('aria-label')).toBe('Space used of the quota of Todo');
		expect(bar?.getAttribute('aria-valuetext')).toBe('5 MB of 50 MB');
		expect(bar?.getAttribute('aria-valuenow')).toBe('10');
		expect(card('Notes').textContent).toContain('FilesNo files yet');
		expect(card('Notes').textContent).toContain('Last writeNot yet');

		expect(text()).toContain('Files of all apps3');
		expect(text()).toContain('Space used by all apps5 MB');
		expect(text()).not.toContain('version');
	});

	it('marks a divergence by the other server, a full quota, and polls them away', async () => {
		apps.mockResolvedValue([
			{
				...todo,
				size: 50 * MB,
				diverged: [{ serverId: 'Laptop000001', name: 'Laptop', since: '2026-11-13T09:00:00Z' }],
			},
		]);
		mountPage(AppsSection);
		await settle();
		expect(text()).toContain('Diverged: choose on a device');
		expect(text()).toContain("The app's files here and on Laptop differ.");
		expect(text()).toContain("The app's files have reached the quota");
		expect(card('Todo').querySelector('bdi')?.textContent).toBe('Todo');
	});

	it('changes the quota in MB, and says so', async () => {
		const setQuota = vi.spyOn(serverApi, 'setQuota').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		byLabel('Change the quota of Todo')?.click();
		await settle();

		expect(dialog().textContent).toContain('The quota of Todo');
		const field = dialog().querySelector('input') as HTMLInputElement;
		expect(field.value).toBe('50');
		field.value = '0';
		field.dispatchEvent(new Event('input'));
		await settle();
		dialog().querySelector('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(setQuota).not.toHaveBeenCalled();
		expect(dialog().textContent).toContain('Enter a whole number, 10 or more.');

		field.value = '200';
		field.dispatchEvent(new Event('input'));
		await settle();
		dialog().querySelector('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(setQuota).toHaveBeenCalledWith('todo', 200 * MB);
		expect(text()).toContain('The quota of Todo is now 200 MB.');
	});

	it("keeps the dialog open with the server's refusal", async () => {
		vi.spyOn(serverApi, 'setQuota').mockRejectedValue(
			new ApiError('followsMain', 'linked to a main'),
		);
		mountPage(AppsSection);
		await settle();
		byLabel('Change the quota of Todo')?.click();
		await settle();
		const field = dialog().querySelector('input') as HTMLInputElement;
		field.value = '60';
		field.dispatchEvent(new Event('input'));
		await settle();
		dialog().querySelector('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(dialog().textContent).toContain(
			'This server is linked to a main server, which sets the quotas.',
		);
	});

	it('changes nothing when the field is left as it was, so a quota of no whole MB stays', async () => {
		apps.mockResolvedValue([{ ...todo, quota: 50 * MB + 123 }]);
		const setQuota = vi.spyOn(serverApi, 'setQuota').mockResolvedValue(null);
		const wrapper = mountPage(AppsSection);
		await settle();
		byLabel('Change the quota of Todo')?.click();
		await settle();
		expect((dialog().querySelector('input') as HTMLInputElement).value).toBe('50');
		dialog().querySelector('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(setQuota).not.toHaveBeenCalled();
		expect(wrapper.findComponent({ name: 'QuotaDialog' }).props('modelValue')).toBe(false);
	});

	it('drops a polling error once the server answers again, and lists the apps without the link', async () => {
		apps.mockRejectedValueOnce(new ApiError('network', ''));
		upstream.mockRejectedValue(new ApiError('internal', 'link'));
		vi.spyOn(serverApi, 'addApp').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		expect(text()).toContain('The server cannot be reached.');

		await openAddDialog();
		const id = document.querySelector('form input') as HTMLInputElement;
		id.value = 'photos';
		id.dispatchEvent(new Event('input'));
		await settle();
		id.closest('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(text()).not.toContain('The server cannot be reached.');
		expect(card('Todo')).toBeTruthy();
	});

	it("opens the app's folder through the server", async () => {
		const openFolder = vi.spyOn(serverApi, 'openFolder').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		byLabel('Open folder of Notes')?.click();
		await settle();
		expect(openFolder).toHaveBeenCalledWith('notes');
		expect(text()).toContain('The folder of Notes opens in the file manager.');
	});

	it("refuses an id Windows reserves for a device, which could not name the app's folder", async () => {
		const addApp = vi.spyOn(serverApi, 'addApp').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		await openAddDialog();
		const id = document.querySelector('form input') as HTMLInputElement;
		id.value = 'con';
		id.dispatchEvent(new Event('input'));
		await settle();
		id.closest('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(text()).toContain('Windows reserves this name for a device');
		expect(addApp).not.toHaveBeenCalled();
	});

	it('lowercases what goes into the ID field, and keeps out what an ID cannot hold', async () => {
		mountPage(AppsSection);
		await settle();
		await openAddDialog();
		const id = document.querySelector('form input') as HTMLInputElement;
		const type = (data: string): InputEvent => {
			const event = new InputEvent('beforeinput', {
				data,
				inputType: 'insertText',
				bubbles: true,
				cancelable: true,
			});
			id.dispatchEvent(event);
			return event;
		};

		expect(type('a').defaultPrevented).toBe(false);
		expect(type('_').defaultPrevented).toBe(true);
		expect(type(' ').defaultPrevented).toBe(true);
		expect(id.value).toBe('');

		// A capital letter lands as a small one.
		expect(type('A').defaultPrevented).toBe(true);
		await settle();
		expect(id.value).toBe('a');

		// A pasted text keeps what an ID can hold.
		id.value = '';
		expect(type('My_App 2').defaultPrevented).toBe(true);
		await settle();
		expect(id.value).toBe('myapp2');
	});

	it('names the least alone while the disk says nothing, and refuses less', async () => {
		const addApp = vi.spyOn(serverApi, 'addApp').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		await openAddDialog();
		expect(text()).toContain('At least 10 MB; you can change it later.');

		const [id, , quota] = [...document.querySelectorAll('form input')] as HTMLInputElement[];
		id.value = 'photos';
		id.dispatchEvent(new Event('input'));
		quota.value = '9';
		quota.dispatchEvent(new Event('input'));
		await settle();
		id.closest('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(addApp).not.toHaveBeenCalled();
		expect(text()).toContain('Enter a whole number, 10 or more.');
	});

	it('caps the size at the free space of the disk, and refuses more', async () => {
		vi.mocked(serverApi.status).mockResolvedValue({ dataFree: 120 * MB } as never);
		const addApp = vi.spyOn(serverApi, 'addApp').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		await openAddDialog();
		expect(text()).toContain('From 10 to 120 MB (no more than the disk has free)');

		const [id, , quota] = [...document.querySelectorAll('form input')] as HTMLInputElement[];
		id.value = 'photos';
		id.dispatchEvent(new Event('input'));
		quota.value = '121';
		quota.dispatchEvent(new Event('input'));
		await settle();
		id.closest('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(addApp).not.toHaveBeenCalled();
		expect(text()).toContain('Enter a whole number from 10 to 120.');
	});

	it('adds an app with its quota in bytes', async () => {
		const addApp = vi.spyOn(serverApi, 'addApp').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		await openAddDialog();
		const inputs = [...document.querySelectorAll('form input')] as HTMLInputElement[];
		const [id, , quota] = inputs;
		expect(quota.value).toBe('50');
		id.value = 'photos';
		id.dispatchEvent(new Event('input'));
		quota.value = '20';
		quota.dispatchEvent(new Event('input'));
		await settle();
		id.closest('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(addApp).toHaveBeenCalledWith({ id: 'photos', quota: 20 * MB });
	});
});

describe('AppsSection on a subordinate', () => {
	beforeEach(() => {
		upstream.mockResolvedValue(asSubordinate);
	});

	it('shows the quota of an app the main lists without changing it, and says the main sets it', async () => {
		apps.mockResolvedValue([{ ...todo, followsMain: true }, notes]);
		mountPage(AppsSection);
		await settle();
		expect(byLabel('Change the quota of Todo')).toBeNull();
		expect(byLabel('Open folder of Todo')).not.toBeNull();
		expect(card('Todo').textContent).toContain(
			"The main server, Desk, sets the quota: change it on the main's page.",
		);
		// An app only this server serves is its own.
		expect(byLabel('Change the quota of Notes')).not.toBeNull();
		expect(card('Notes').textContent).not.toContain('sets the quota');
		// A new app takes a quota, which the main's replaces if it serves the app too.
		await openAddDialog();
		expect([...document.querySelectorAll('form input')]).toHaveLength(3);
		expect(text()).toContain(
			"If the main server serves an app with this ID too, the main's quota replaces it.",
		);
	});

	it('names the main it diverged from, and a sync the quota holds', async () => {
		apps.mockResolvedValue([
			{
				...todo,
				diverged: [{ serverId: 'MainServer01', name: 'Desk', since: '2026-11-13T09:00:00Z' }],
				failing: 'quota',
				followsMain: true,
			},
		]);
		mountPage(AppsSection);
		await settle();
		expect(text()).toContain('Diverged: choose on a device');
		expect(text()).toContain("The app's files here and on Desk differ.");
		expect(text()).toContain('Held by the quota');
		expect(text()).toContain("Raise the quota on the main's page, or remove files in the app");
		// One app: no totals.
		expect(text()).not.toContain('Files of all apps');
	});
});
