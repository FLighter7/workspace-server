// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { defineComponent, h } from 'vue';
import { ApiError, serverApi } from '@/api/client';
import type { DeviceInfo, UpstreamStatus } from '@/api/types';
import ConfirmDialog from '@/components/ConfirmDialog.vue';
import { resetNotices, noticeState } from '@/composables/useNotify';
import { invitedCode, ownCode } from '@/composables/usePairingCode';
import { mountPage, settle } from '@/testing/mountPage';
import DevicesSection from '../DevicesSection.vue';

/** The section with the page's one confirmation dialog, as App.vue holds it. */
const Page = defineComponent({ render: () => h('div', [h(DevicesSection), h(ConfirmDialog)]) });

const phone: DeviceInfo = {
	id: 'Phone00000000001',
	app: 'todo',
	name: 'iPhone Safari (PWA)',
	machine: 'Machine000000001',
	pairedAt: '2026-11-10T10:00:00Z',
	lastSeen: '2026-11-12T10:00:00Z',
	connected: true,
};

/** Another app on the same phone. */
const phoneNotes: DeviceInfo = {
	id: 'Phone00000000002',
	app: 'notes',
	name: 'iPhone Safari',
	machine: 'Machine000000001',
	pairedAt: '2026-11-11T10:00:00Z',
	lastSeen: '2026-11-11T12:00:00Z',
	connected: false,
};

const desk: DeviceInfo = {
	id: 'Desk000000000001',
	app: 'todo',
	name: 'Windows Edge',
	machine: 'Machine000000002',
	pairedAt: '2026-11-10T10:00:00Z',
	lastSeen: '2026-11-12T09:00:00Z',
	connected: false,
};

const laptop: DeviceInfo = {
	id: 'Server0000000001',
	app: '',
	kind: 'server',
	name: 'Laptop pairing name',
	pairedAt: '2026-11-11T10:00:00Z',
	lastSeen: '2026-11-12T10:00:00Z',
	connected: false,
	server: {
		serverId: 'Lp7tOp0123ab',
		name: 'Laptop',
		port: 47801,
		hosts: ['5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f.local', '192.168.1.30'],
	},
};

const asMain: UpstreamStatus = {
	linked: false,
	connected: false,
	halted: false,
	apps: [],
	subordinates: 1,
	term: 0,
};

const text = (): string => document.body.textContent ?? '';

const button = (label: string, within: ParentNode = document): HTMLButtonElement =>
	[...within.querySelectorAll('button')].find(
		(b) => b.textContent?.trim() === label,
	) as HTMLButtonElement;

const headings = (level: string): string[] =>
	[...document.querySelectorAll(level)].map(
		(e) => e.textContent?.replace(/\s+/g, ' ').trim() ?? '',
	);

beforeEach(() => {
	vi.spyOn(serverApi, 'apps').mockResolvedValue([
		{
			id: 'todo',
			name: 'Todo',
			quota: 52_428_800,
			files: 0,
			size: 0,
			modified: null,
			devices: 1,
			folder: 'C:\\ws\\WSS-Data\\apps\\todo',
			diverged: [],
			followsMain: false,
		},
	]);
	vi.spyOn(serverApi, 'devices').mockResolvedValue([laptop, phone]);
	vi.spyOn(serverApi, 'machines').mockResolvedValue([
		{ id: 'Machine000000001', name: "Alex's phone" },
		{ id: 'Machine000000002', name: 'Desk' },
	]);
});

const labelled = (label: string, within: ParentNode = document): HTMLButtonElement =>
	within.querySelector(`button[aria-label="${label}"]`) as HTMLButtonElement;

afterEach(() => {
	vi.restoreAllMocks();
	invitedCode.drop();
	ownCode.drop();
	resetNotices();
	document.body.innerHTML = '';
});

describe('DevicesSection on a main', () => {
	beforeEach(() => {
		vi.spyOn(serverApi, 'upstream').mockResolvedValue(asMain);
	});

	it('marks a linked server as one, with what it announced', async () => {
		mountPage(Page);
		await settle();
		expect(headings('h2')).toEqual(['Linked servers', 'Devices']);
		expect(headings('h3')).toEqual(['Laptop', "Alex's phone"]);
		expect(text()).toContain('Lp7tOp0123ab');
		expect(text()).toContain('192.168.1.30');
		expect(text()).toContain('47801');
		expect(button('Pair')).toBeDefined();
	});

	it('says what removing a server does', async () => {
		const remove = vi.spyOn(serverApi, 'removeDevice').mockResolvedValue(null);
		mountPage(Page);
		await settle();
		button('Remove').click();
		await settle();
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		expect(dialog.textContent).toContain('Remove Laptop?');
		expect(dialog.textContent).toContain('The server on that computer stops syncing with this one');
		button('Remove', dialog).click();
		await settle();
		expect(remove).toHaveBeenCalledWith('Server0000000001');
	});

	it('shows each device once, with its apps, by name', async () => {
		vi.spyOn(serverApi, 'devices').mockResolvedValue([laptop, phone, phoneNotes, desk]);
		mountPage(Page);
		await settle();
		expect(headings('h2')).toEqual(['Linked servers', 'Devices']);
		expect(headings('h3')).toEqual(['Laptop', "Alex's phone", 'Desk']);
		expect(labelled("Apps on Alex's phone: 2, connected now: 1")).not.toBeNull();
		expect(labelled('Apps on Desk: 1')).not.toBeNull();
		expect(text()).toContain('Connected now');
	});

	it("lists a device's apps, and moves one to a device of its own", async () => {
		vi.spyOn(serverApi, 'devices').mockResolvedValue([phone, phoneNotes]);
		const move = vi
			.spyOn(serverApi, 'moveDevice')
			.mockResolvedValue({ machine: 'Machine000000003' });
		mountPage(Page);
		await settle();
		labelled("Apps on Alex's phone: 2, connected now: 1").click();
		await settle();
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		expect(dialog.textContent).toContain("Apps on Alex's phone");
		expect(headings('h3')).toEqual(["Alex's phone", 'Todo', 'notes']);
		expect(dialog.textContent).toContain('iPhone Safari (PWA)');
		labelled('Move notes to another device', dialog).click();
		await settle();
		const item = [...document.querySelectorAll('.v-list-item')].find(
			(e) => e.textContent?.trim() === 'A device of its own',
		) as HTMLElement;
		item.click();
		await settle();
		expect(move).toHaveBeenCalledWith('Phone00000000002', '');
		expect(noticeState.current?.key).toBe('moved');
	});

	it('deletes a device with all its apps after a question, and says so', async () => {
		const remove = vi.spyOn(serverApi, 'removeMachine').mockResolvedValue(null);
		mountPage(Page);
		await settle();
		labelled("Delete Alex's phone with all its apps").click();
		await settle();
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		expect(dialog.textContent).toContain("Delete Alex's phone?");
		expect(dialog.textContent).toContain('Every app on this device loses access.');
		button('Delete', dialog).click();
		await settle();
		expect(remove).toHaveBeenCalledWith('Machine000000001');
		expect(document.body.textContent).toContain('Deleted');
	});

	it('renames a device', async () => {
		const rename = vi.spyOn(serverApi, 'renameMachine').mockResolvedValue(null);
		mountPage(Page);
		await settle();
		labelled("Rename Alex's phone").click();
		await settle();
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		const input = dialog.querySelector('input') as HTMLInputElement;
		expect(input.value).toBe("Alex's phone");
		input.value = 'Work phone';
		input.dispatchEvent(new Event('input'));
		await settle();
		button('Save', dialog).click();
		await settle();
		expect(rename).toHaveBeenCalledWith('Machine000000001', 'Work phone');
		expect(text()).toContain('Renamed.');
	});

	it('hands the role of main to a linked server, after a question', async () => {
		const handOver = vi.spyOn(serverApi, 'handOver').mockResolvedValue({ done: true });
		mountPage(Page);
		await settle();
		button('Make it the main').click();
		await settle();
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		expect(dialog.textContent).toContain('Make Laptop the main?');
		expect(dialog.textContent).toContain('Devices follow by themselves, without pairing again.');
		button('Make it the main', dialog).click();
		await settle();
		expect(handOver).toHaveBeenCalledWith('Lp7tOp0123ab');
		expect(text()).toContain('Laptop is the main now; this server syncs through it.');
	});

	it('says when the server to hand the role to is not connected', async () => {
		vi.spyOn(serverApi, 'handOver').mockRejectedValue(new ApiError('notConnected', 'away'));
		mountPage(Page);
		await settle();
		button('Make it the main').click();
		await settle();
		button('Make it the main', document.querySelector('[role="dialog"]') as HTMLElement).click();
		await settle();
		expect(text()).toContain('That server is not connected now.');
	});

	it('shows a server that has not announced itself by its pairing name', async () => {
		vi.spyOn(serverApi, 'devices').mockResolvedValue([{ ...laptop, server: undefined }]);
		mountPage(Page);
		await settle();
		expect(headings('h3')).toEqual(['Laptop pairing name']);
		expect(text()).toContain('Not connected yet');
		expect(button('Make it the main')).toBeUndefined();
	});
});

describe('DevicesSection on a subordinate', () => {
	beforeEach(() => {
		vi.spyOn(serverApi, 'upstream').mockResolvedValue({
			linked: true,
			name: 'Desk',
			serverId: 'MainServer01',
			connected: true,
			halted: false,
			apps: ['todo'],
			subordinates: 0,
			term: 0,
		});
		vi.spyOn(serverApi, 'devices').mockResolvedValue([]);
	});

	it('points to the main instead of issuing codes that would be refused', async () => {
		const issue = vi.spyOn(serverApi, 'issuePairing');
		mountPage(Page);
		await settle();
		expect(button('Pair')).toBeUndefined();
		expect(text()).toContain('This server is linked to the main server Desk');
		expect(button("Pair this computer's browser")).toBeDefined();
		expect(issue).not.toHaveBeenCalled();
	});

	it("shows the main's code for this computer's browser", async () => {
		const invite = vi.spyOn(serverApi, 'invite').mockResolvedValue({
			code: 'wsrv1.main-code',
			expiresAt: new Date(Date.now() + 600_000).toISOString(),
			qr: 'data:image/png;base64,iVBORw0KGgo=',
		});
		mountPage(Page);
		await settle();
		button("Pair this computer's browser").click();
		await settle();
		expect(invite).toHaveBeenCalledTimes(1);
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		expect(dialog.textContent).toContain('This is a pairing code of the main server, Desk.');
		expect(dialog.querySelector('img')?.getAttribute('alt')).toBe('The pairing code as a QR code');
		expect(dialog.querySelector('textarea')?.value).toBe('wsrv1.main-code');
	});

	it('says that only a reachable main issues a code', async () => {
		vi.spyOn(serverApi, 'invite').mockRejectedValue(
			new ApiError('upstream', 'away', 'unreachable'),
		);
		mountPage(Page);
		await settle();
		button("Pair this computer's browser").click();
		await settle();
		expect(text()).toContain(
			'The main server cannot be reached now, and only it issues pairing codes.',
		);
		expect(document.querySelector('[role="dialog"] img')).toBeNull();
	});
});
