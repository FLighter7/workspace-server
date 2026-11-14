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

describe('AppsSection on a main', () => {
	it("shows each app's files, the space they take of the quota, its folder and the totals", async () => {
		mountPage(AppsSection);
		await settle();

		const first = card('Todo').textContent ?? '';
		expect(first).toContain('3');
		expect(first).toContain('5 MB of 50 MB');
		expect(first).toContain('C:\\ws\\WSS-Data\\apps\\todo');
		const bar = card('Todo').querySelector('[role="progressbar"][aria-hidden="false"]');
		expect(bar?.getAttribute('aria-label')).toBe('Space used of the quota');
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
		expect(dialog().textContent).toContain('Enter a whole number from 1 to 1,048,576.');

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
		dialog().querySelector('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(dialog().textContent).toContain(
			'This server is linked to a main server, which sets the quotas.',
		);
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

	it('adds an app with its quota in bytes', async () => {
		const addApp = vi.spyOn(serverApi, 'addApp').mockResolvedValue(null);
		mountPage(AppsSection);
		await settle();
		const inputs = [...document.querySelectorAll('form input')] as HTMLInputElement[];
		const [id, , quota] = inputs;
		expect(quota.value).toBe('50');
		id.value = 'photos';
		id.dispatchEvent(new Event('input'));
		quota.value = '2';
		quota.dispatchEvent(new Event('input'));
		await settle();
		id.closest('form')?.dispatchEvent(new Event('submit'));
		await settle();
		expect(addApp).toHaveBeenCalledWith({ id: 'photos', quota: 2 * MB });
	});
});

describe('AppsSection on a subordinate', () => {
	beforeEach(() => {
		upstream.mockResolvedValue(asSubordinate);
	});

	it('shows the quota without changing it, and says the main sets it', async () => {
		apps.mockResolvedValue([todo]);
		mountPage(AppsSection);
		await settle();
		expect(byLabel('Change the quota of Todo')).toBeNull();
		expect(byLabel('Open folder of Todo')).not.toBeNull();
		expect(text()).toContain(
			"The main server, Desk, sets the quota: change it on the main's page.",
		);
		expect([...document.querySelectorAll('form input')]).toHaveLength(2);
	});

	it('names the main it diverged from, and a sync the quota holds', async () => {
		apps.mockResolvedValue([
			{
				...todo,
				diverged: [{ serverId: 'MainServer01', name: 'Desk', since: '2026-11-13T09:00:00Z' }],
				failing: 'quota',
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
