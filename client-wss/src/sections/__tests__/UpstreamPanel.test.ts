// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { defineComponent, h } from 'vue';
import { ApiError, serverApi } from '@/api/client';
import type { UpstreamStatus } from '@/api/types';
import ConfirmDialog from '@/components/ConfirmDialog.vue';
import { mountPage, settle } from '@/testing/mountPage';
import UpstreamPanel from '../UpstreamPanel.vue';

// The camera: a stand-in that hands the test the function it calls with
// every code it reads.
const camera = vi.hoisted(() => ({
	decode: null as ((result: { data: string }) => void) | null,
	destroyed: 0,
}));
vi.mock('qr-scanner', () => ({
	default: class {
		constructor(_video: HTMLVideoElement, decode: (result: { data: string }) => void) {
			camera.decode = decode;
		}
		start(): Promise<void> {
			return Promise.resolve();
		}
		destroy(): void {
			camera.destroyed++;
		}
	},
}));

/** The panel with the page's one confirmation dialog, as App.vue holds it. */
const Page = defineComponent({ render: () => h('div', [h(UpstreamPanel), h(ConfirmDialog)]) });

const asMain: UpstreamStatus = {
	linked: false,
	connected: false,
	halted: false,
	apps: [],
	subordinates: 0,
};

const linked: UpstreamStatus = {
	linked: true,
	serverId: 'MainServer01',
	name: 'Desk',
	hosts: ['192.168.1.10'],
	linkedAt: '2026-11-12T09:00:00Z',
	connected: true,
	halted: false,
	lastSync: '2026-11-12T09:05:00Z',
	apps: ['notes', 'todo'],
	devices: 2,
	subordinates: 0,
};

const text = (): string => document.body.textContent ?? '';

const button = (label: string, within: ParentNode = document): HTMLButtonElement =>
	[...within.querySelectorAll('button')].find(
		(b) => b.textContent?.trim() === label,
	) as HTMLButtonElement;

async function paste(code: string): Promise<void> {
	const field = document.querySelector('textarea') as HTMLTextAreaElement;
	field.value = code;
	field.dispatchEvent(new Event('input'));
	await settle();
	field.closest('form')?.dispatchEvent(new Event('submit'));
	await settle();
}

let upstream: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
	camera.decode = null;
	camera.destroyed = 0;
	upstream = vi.spyOn(serverApi, 'upstream').mockResolvedValue(asMain);
});

afterEach(() => {
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('UpstreamPanel on a main', () => {
	it('links by a pasted code and shows the link', async () => {
		const link = vi.spyOn(serverApi, 'link').mockResolvedValue(linked);
		mountPage(Page);
		await settle();
		expect(text()).toContain('This server is a main');
		expect(text()).toContain('Nothing is deleted.');

		await paste('  wsrv1.code  ');
		expect(link).toHaveBeenCalledWith('wsrv1.code');
		expect(text()).toContain('Linked to Desk.');
		expect(text()).toContain('MainServer01');
		expect(text()).toContain('Connected');
		expect([...document.querySelectorAll('code')].map((c) => c.textContent)).toContain('todo');
		expect(document.querySelector('textarea')).toBeNull();
		expect(document.activeElement?.id).toBe('upstream-h');
	});

	it("says why the main refused, with the server's detail", async () => {
		vi.spyOn(serverApi, 'link').mockRejectedValue(
			new ApiError('upstream', 'used (badSecret)', 'badSecret'),
		);
		mountPage(Page);
		await settle();
		await paste('wsrv1.code');
		expect(text()).toContain(
			'The code is used or has expired. Get a new code from the main. (used (badSecret))',
		);
		expect(document.querySelector('textarea')).not.toBeNull();
	});

	it('links by a code the camera reads, and stops the camera', async () => {
		const link = vi.spyOn(serverApi, 'link').mockResolvedValue(linked);
		mountPage(Page);
		await settle();
		button('Scan with the camera').click();
		await settle();
		expect(button('Stop the camera').getAttribute('aria-pressed')).toBe('true');
		expect(text()).toContain("Hold the main's QR code in front of the camera.");

		camera.decode?.({ data: 'wsrv1.scanned' });
		camera.decode?.({ data: 'wsrv1.scanned' }); // the next frame, while linking
		await settle();
		expect(link).toHaveBeenCalledTimes(1);
		expect(link).toHaveBeenCalledWith('wsrv1.scanned');
		expect(camera.destroyed).toBe(1);
		expect(text()).toContain('Linked to Desk.');
	});

	it('offers no link while other servers are linked to this one', async () => {
		upstream.mockResolvedValue({ ...asMain, subordinates: 1 });
		mountPage(Page);
		await settle();
		expect(text()).toContain('Other servers are linked to this one');
		expect(document.querySelector('textarea')).toBeNull();
		expect(button('Link')).toBeUndefined();
	});
});

describe('UpstreamPanel on a subordinate', () => {
	it('shows a halted link, the stopped apps and that a restart resumes them, and offers no code', async () => {
		upstream.mockResolvedValue({
			...linked,
			connected: false,
			halted: true,
			reason: 'badToken',
			apps: ['notes'],
			stopped: ['todo'],
		});
		mountPage(Page);
		await settle();
		expect(text()).toContain('Stopped');
		expect(text()).toContain('The main removed this server.');
		expect(text()).toContain('Stopped apps');
		expect(text()).toContain('A restart of this server resumes them.');
		// A halted link reaches no main, which alone issues codes.
		expect(button("Pair this computer's browser")).toBeUndefined();
		expect(button('Unlink')).toBeDefined();
	});

	it('says that a main out of reach is retried', async () => {
		upstream.mockResolvedValue({ ...linked, connected: false, lastSync: undefined });
		mountPage(Page);
		await settle();
		expect(text()).toContain('Not connected');
		expect(text()).toContain('The main is out of reach now.');
		expect(text()).toContain('Not yet');
	});

	it('unlinks after a question that says the data stays', async () => {
		upstream.mockResolvedValue(linked);
		const unlink = vi.spyOn(serverApi, 'unlink').mockResolvedValue({ forgotten: true });
		mountPage(Page);
		await settle();

		button('Unlink').click();
		await settle();
		const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
		expect(dialog.textContent).toContain('Unlink from Desk?');
		expect(dialog.textContent).toContain('No data is deleted');
		button('Cancel', dialog).click();
		await settle();
		expect(unlink).not.toHaveBeenCalled();

		button('Unlink').click();
		await settle();
		upstream.mockResolvedValue(asMain);
		button('Unlink', document.querySelector('[role="dialog"]') as HTMLElement).click();
		await settle();
		expect(unlink).toHaveBeenCalledTimes(1);
		expect(text()).toContain('Unlinked. This server is a main again.');
		expect(document.querySelector('textarea')).not.toBeNull();
	});

	it('says when the main could not be told', async () => {
		upstream.mockResolvedValue(linked);
		vi.spyOn(serverApi, 'unlink').mockResolvedValue({ forgotten: false });
		mountPage(Page);
		await settle();
		button('Unlink').click();
		await settle();
		button('Unlink', document.querySelector('[role="dialog"]') as HTMLElement).click();
		await settle();
		expect(text()).toContain("remove it on the main's page, under Devices.");
	});
});
