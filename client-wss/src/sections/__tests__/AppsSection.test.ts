// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, serverApi } from '@/api/client';
import type { AppInfo, DeviceInfo, UpstreamStatus } from '@/api/types';
import { noticeState, resetNotices } from '@/composables/useNotify';
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

/** Todo's two devices, one of them connected now, and a linked server, which is no app's. */
const todoDevices: DeviceInfo[] = [
	{
		id: 'PhoneDevice0001',
		app: 'todo',
		name: 'Pixel 8',
		pairedAt: '2026-11-03T09:00:00Z',
		lastSeen: '2026-11-13T10:00:00Z',
		connected: true,
	},
	{
		id: 'LaptopDevice001',
		app: 'todo',
		name: 'Laptop',
		pairedAt: '2026-11-04T09:00:00Z',
		lastSeen: '2026-11-12T18:00:00Z',
		connected: false,
	},
	{
		id: 'LinkedServer001',
		app: '',
		kind: 'server',
		name: 'Laptop server',
		pairedAt: '2026-11-05T09:00:00Z',
		lastSeen: '2026-11-13T10:00:00Z',
		connected: true,
	},
];

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
	vi.spyOn(serverApi, 'devices').mockResolvedValue(todoDevices);
});

afterEach(() => {
	vi.restoreAllMocks();
	resetNotices();
	document.body.innerHTML = '';
});

const text = (): string => document.body.textContent?.replace(/\s+/g, ' ') ?? '';

const byLabel = (label: string): HTMLButtonElement | null =>
	document.querySelector(`button[aria-label="${label}"]`);

/** The Open folder icon of an app: its name is "Open folder of <app>: <path>". */
const folderButton = (name: string): HTMLButtonElement | null =>
	document.querySelector(`button[aria-label^="Open folder of ${name}"]`);

/** The card of the app whose heading names it. */
const card = (name: string): HTMLElement =>
	[...document.querySelectorAll('li')].find((li) =>
		li.querySelector('h2')?.textContent?.includes(name),
	) as HTMLElement;

/** The value of an app's fact, by the fact's label. */
const fact = (name: string, label: string): string | undefined =>
	[...card(name).querySelectorAll('dt')]
		.find((dt) => dt.textContent?.trim() === label)
		?.nextElementSibling?.textContent?.trim();

const dialog = (): HTMLElement => document.querySelector('[role="dialog"]') as HTMLElement;

/** The button at the top of the page, which opens the form for a new app. */
async function openAddDialog(): Promise<void> {
	byLabel('Add an app')?.click();
	await settle();
}

describe('AppsSection on a main', () => {
	it("shows the space each app's files take of the quota, its folder and the total", async () => {
		mountPage(AppsSection);
		await settle();

		const first = card('Todo').textContent ?? '';
		expect(first).toContain('5 MB of 50 MB');
		// The folder's path is not shown: it is the Open folder icon's name and tooltip.
		expect(first).not.toContain('C:\\ws');
		expect(folderButton('Todo')?.title).toBe('Open folder: C:\\ws\\WSS-Data\\apps\\todo');
		const bar = card('Todo').querySelector('[role="progressbar"][aria-hidden="false"]');
		expect(bar?.getAttribute('aria-label')).toBe('Space used of the quota of Todo');
		expect(bar?.getAttribute('aria-valuetext')).toBe('5 MB of 50 MB');
		expect(bar?.getAttribute('aria-valuenow')).toBe('10');
		expect(fact('Notes', 'Last write')).toBe('Not yet');
		expect(byLabel('Devices of Todo: 2, connected now: 1')?.textContent).toBe('21 connected');
		expect(fact('Notes', 'Devices')).toBe('0');
		// The number of files is not shown: it says nothing the space does not.
		expect(fact('Todo', 'Files')).toBeUndefined();

		expect(text()).toContain('Space used by all apps5 MB');
		expect(text()).not.toContain('Files of all apps');
		expect(text()).not.toContain('version');
	});

	it('lays a card out as the name and ID with Open folder and Delete, the space with Change, then the facts', async () => {
		mountPage(AppsSection);
		await settle();

		const todoCard = card('Todo');
		const items = [
			...todoCard.querySelectorAll(
				'code, h2, button, [role="progressbar"][aria-hidden="false"], dt',
			),
		].map((el) => el.getAttribute('aria-label') ?? el.textContent?.trim());
		expect(items).toEqual([
			'Todo',
			'todo',
			// Open folder is an icon, named with the path.
			'Open folder of Todo: C:\\ws\\WSS-Data\\apps\\todo',
			'Delete Todo',
			'Space used of the quota of Todo',
			'Change the quota of Todo',
			// The facts, two tiles: the last write, the devices, a button that lists them.
			'Last write',
			'Devices',
			'Devices of Todo: 2, connected now: 1',
		]);
		// Nothing is folded away: there is no toggle.
		expect(todoCard.querySelector('[aria-expanded]')).toBeNull();
	});

	it("lists an app's devices in a dialog from its Devices tile", async () => {
		mountPage(AppsSection);
		await settle();
		byLabel('Devices of Todo: 2, connected now: 1')?.click();
		await settle();
		expect(dialog().getAttribute('aria-labelledby')).toBeTruthy();
		expect(dialog().querySelector('h2')?.textContent?.trim()).toBe('Devices of Todo');
		const rows = [...dialog().querySelectorAll('li')].map((li) =>
			li.textContent?.replace(/\s+/g, ' ').trim(),
		);
		expect(rows).toHaveLength(2);
		expect(rows[0]).toContain('Pixel 8');
		expect(rows[0]).toContain('Connected now');
		expect(rows[0]).toContain('Paired Nov 3,');
		expect(rows[0]).not.toContain('Last seen');
		// Not connected: when it was last seen stands in for the state, named for a screen reader.
		expect(rows[1]).toContain('Laptop');
		expect(rows[1]).toContain('Last seen: Nov 12,');
		expect(rows[1]).not.toContain('Not connected');
		expect(byLabel('Disconnect Laptop')?.getAttribute('title')).toBe('Disconnect');
		// The linked server is no app's device.
		expect(dialog().textContent).not.toContain('Laptop server');
	});

	it('lists the connected devices first, then the latest seen first', async () => {
		const at = (name: string, lastSeen: string, connected: boolean): DeviceInfo => ({
			id: `${name.padEnd(15, '0')}`,
			app: 'todo',
			name,
			pairedAt: '2026-10-01T09:00:00Z',
			lastSeen,
			connected,
		});
		vi.mocked(serverApi.devices).mockResolvedValue([
			at('OldOffline', '2026-10-05T09:00:00Z', false),
			at('Online', '2026-11-13T09:00:00Z', true),
			at('NewOffline', '2026-11-12T09:00:00Z', false),
			at('LaterOnline', '2026-11-13T10:00:00Z', true),
			at('MidOffline', '2026-11-01T09:00:00Z', false),
		]);
		mountPage(AppsSection);
		await settle();
		byLabel('Devices of Todo: 5, connected now: 2')?.click();
		await settle();
		const names = [...dialog().querySelectorAll('h3')].map((h) => h.textContent?.trim());
		expect(names).toEqual(['LaterOnline', 'Online', 'NewOffline', 'MidOffline', 'OldOffline']);
	});

	it('keeps the Devices tile of an app with no devices plain: there is nothing to list', async () => {
		mountPage(AppsSection);
		await settle();
		const tile = [...card('Notes').querySelectorAll('dt')].find(
			(dt) => dt.textContent?.trim() === 'Devices',
		)?.parentElement;
		expect(tile?.querySelector('button')).toBeNull();
		expect(tile?.querySelector('svg')).toBeNull();
		expect(byLabel('Devices of Notes: 0, connected now: 0')).toBeNull();
	});

	/** The server's removal: the device leaves the list the next poll reads. */
	function serverRemoves(): ReturnType<typeof vi.spyOn> {
		let list = [...todoDevices];
		vi.mocked(serverApi.devices).mockImplementation(async () => list);
		return vi.spyOn(serverApi, 'removeDevice').mockImplementation(async (id: string) => {
			list = list.filter((d) => d.id !== id);
			return null;
		});
	}

	it('removes a device from the dialog, after a question, and says so in the snackbar', async () => {
		const removeDevice = serverRemoves();
		const confirm = await import('@/composables/useConfirm');
		vi.spyOn(confirm, 'ask').mockResolvedValue(true);
		mountPage(AppsSection);
		await settle();
		byLabel('Devices of Todo: 2, connected now: 1')?.click();
		await settle();
		byLabel('Disconnect Laptop')?.click();
		await settle();
		expect(confirm.ask).toHaveBeenCalledWith(
			expect.objectContaining({ title: 'disconnectTitle', name: 'Laptop', ok: 'disconnect' }),
		);
		expect(removeDevice).toHaveBeenCalledWith('LaptopDevice001');
		expect(noticeState.current).toEqual({ key: 'disconnected' });
		// Gone at once, before the next poll.
		expect(byLabel('Disconnect Laptop')).toBeNull();
		expect(byLabel('Disconnect Pixel 8')).not.toBeNull();
		expect(byLabel('Devices of Todo: 1, connected now: 1')).not.toBeNull();
	});

	it('closes the dialog once its last device is removed, and the tile turns plain', async () => {
		serverRemoves();
		const confirm = await import('@/composables/useConfirm');
		vi.spyOn(confirm, 'ask').mockResolvedValue(true);
		mountPage(AppsSection);
		await settle();
		byLabel('Devices of Todo: 2, connected now: 1')?.click();
		await settle();
		byLabel('Disconnect Laptop')?.click();
		await settle();
		expect(dialog()).not.toBeNull();
		byLabel('Disconnect Pixel 8')?.click();
		await settle();
		expect(document.querySelector('.v-overlay--active [role="dialog"]')).toBeNull();
		expect(fact('Todo', 'Devices')).toBe('0');
		expect(byLabel('Devices of Todo: 0, connected now: 0')).toBeNull();
		expect(document.body.textContent).not.toContain('No devices yet');
	});

	it('keeps a device the user did not confirm removing', async () => {
		const removeDevice = vi.spyOn(serverApi, 'removeDevice').mockResolvedValue(null);
		const confirm = await import('@/composables/useConfirm');
		vi.spyOn(confirm, 'ask').mockResolvedValue(false);
		mountPage(AppsSection);
		await settle();
		byLabel('Devices of Todo: 2, connected now: 1')?.click();
		await settle();
		byLabel('Disconnect Laptop')?.click();
		await settle();
		expect(removeDevice).not.toHaveBeenCalled();
		expect(byLabel('Disconnect Laptop')).not.toBeNull();
	});

	it('counts the space in the unit that fits: MB, GB, TB', async () => {
		apps.mockResolvedValue([
			{ ...todo, size: 3 * 1024 ** 3, quota: 2 * 1024 ** 4 },
			{ ...notes, id: 'photos', name: 'Photos', size: 700 * MB, quota: 2 * 1024 ** 3 },
		]);
		mountPage(AppsSection);
		await settle();
		expect(card('Todo').textContent).toContain('3 GB of 2 TB');
		expect(card('Photos').textContent).toContain('700 MB of 2 GB');
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
		// Said in the page's snackbar, not in the section.
		expect(noticeState.current).toEqual({
			key: 'quotaSaved',
			values: { app: 'Todo', quota: '200 MB' },
		});
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
		const button = folderButton('Notes');
		expect(button?.getAttribute('aria-label')).toBe(
			'Open folder of Notes: C:\\ws\\WSS-Data\\apps\\notes',
		);
		button?.click();
		await settle();
		expect(openFolder).toHaveBeenCalledWith('notes');
		// The window is the answer: the page says nothing.
		expect(noticeState.current).toBeNull();
		expect(text()).not.toContain('opens in the file manager');
	});

	it('says in the snackbar why the folder did not open, and nothing in the section', async () => {
		vi.spyOn(serverApi, 'openFolder').mockRejectedValue(new ApiError('notFound', 'no such app'));
		mountPage(AppsSection);
		await settle();
		folderButton('Notes')?.click();
		await settle();
		expect(noticeState.current).toEqual({ key: 'errNotFound', detail: 'no such app', error: true });
		expect(document.querySelector('[role="status"]')?.textContent?.trim()).toBe('');
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
		expect(text()).toContain('At least 10 MB. You can change it later.');

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
		expect(folderButton('Todo')).not.toBeNull();
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
		expect(text()).not.toContain('Space used by all apps');
	});
});
