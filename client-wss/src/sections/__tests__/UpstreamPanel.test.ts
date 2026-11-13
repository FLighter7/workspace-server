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
	term: 0,
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
	term: 0,
};

const text = (): string => document.body.textContent ?? '';

/** The Resume button of a stopped app, found by its accessible name. */
const resumeButton = (app: string): HTMLButtonElement | null =>
	document.querySelector(`button[aria-label="Resume ${app}"]`);

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

	it('names the servers it waits for after it took the role of main over', async () => {
		upstream.mockResolvedValue({
			...asMain,
			subordinates: 2,
			term: 1,
			waiting: [{ serverId: 'MainServer01', name: 'Desk' }],
		});
		mountPage(Page);
		await settle();
		expect(text()).toContain('This server took over as the main.');
		expect(text()).toContain('Desk');
		expect(text()).toContain('A server that is gone for good can be removed under Devices.');
	});
});

describe('UpstreamPanel making this server the main', () => {
	const dialog = (): HTMLElement => document.querySelector('[role="dialog"]') as HTMLElement;

	it('asks, then switches while the main is reachable', async () => {
		upstream.mockResolvedValue(linked);
		const promote = vi.spyOn(serverApi, 'promote').mockResolvedValue({ followed: true });
		mountPage(Page);
		await settle();

		button('Make this server the main').click();
		await settle();
		expect(dialog().textContent).toContain('Make this server the main?');
		expect(dialog().textContent).toContain('Devices follow by themselves, without pairing again.');
		button('Cancel', dialog()).click();
		await settle();
		expect(promote).not.toHaveBeenCalled();

		button('Make this server the main').click();
		await settle();
		upstream.mockResolvedValue({ ...asMain, subordinates: 1, term: 1 });
		button('Make it the main', dialog()).click();
		await settle();
		expect(promote).toHaveBeenCalledWith(false);
		expect(text()).toContain('This server is the main now, and Desk syncs through it.');
		expect(document.activeElement?.id).toBe('upstream-h');
	});

	it('asks again, saying what happens next, when the main turns out to be away', async () => {
		upstream.mockResolvedValue(linked);
		const promote = vi
			.spyOn(serverApi, 'promote')
			.mockRejectedValueOnce(new ApiError('upstream', 'away', 'unreachable'))
			.mockResolvedValueOnce({ followed: false });
		mountPage(Page);
		await settle();

		button('Make this server the main').click();
		await settle();
		button('Make it the main', dialog()).click();
		await settle();
		expect(dialog().textContent).toContain(
			'Desk is out of reach. Make this server the main anyway?',
		);
		expect(dialog().textContent).toContain('both act as main until they meet');
		expect(dialog().textContent).toContain('nothing is lost');
		expect(dialog().textContent).toContain('the old main steps down by itself');
		button('Make it the main anyway', dialog()).click();
		await settle();
		expect(promote.mock.calls).toEqual([[false], [true]]);
		expect(text()).toContain('The old main steps down when the two meet');
	});

	it('goes straight to the forced question while the main is out of reach', async () => {
		upstream.mockResolvedValue({ ...linked, connected: false });
		const promote = vi.spyOn(serverApi, 'promote').mockResolvedValue({ followed: false });
		mountPage(Page);
		await settle();

		button('Make this server the main').click();
		await settle();
		expect(dialog().textContent).toContain('Desk is out of reach.');
		button('Cancel', dialog()).click();
		await settle();
		expect(promote).not.toHaveBeenCalled();
	});

	it('says when the documents are not the same yet', async () => {
		upstream.mockResolvedValue(linked);
		vi.spyOn(serverApi, 'promote').mockRejectedValue(new ApiError('notInStep', 'not yet'));
		mountPage(Page);
		await settle();
		button('Make this server the main').click();
		await settle();
		button('Make it the main', dialog()).click();
		await settle();
		expect(text()).toContain('The documents are not yet the same on this server and the main.');
	});

	it('says that it looks for the new main when its main stepped down', async () => {
		upstream.mockResolvedValue({ ...linked, connected: false, reason: 'mainSteppedDown' });
		mountPage(Page);
		await settle();
		expect(text()).toContain('Looking for the new main');
		expect(text()).toContain('The main handed its role to another server.');
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
		// A halted link syncs nothing, so it resumes nothing either.
		expect(resumeButton('todo')).toBeNull();
		expect(text()).not.toContain('“Resume”');
		// A halted link reaches no main, which alone issues codes.
		expect(button("Pair this computer's browser")).toBeUndefined();
		expect(button('Unlink')).toBeDefined();
	});

	it('resumes an app the write guard stopped, at once and without a question', async () => {
		upstream.mockResolvedValue({ ...linked, apps: ['notes'], stopped: ['todo', 'work'] });
		const resume = vi
			.spyOn(serverApi, 'resume')
			.mockResolvedValue({ ...linked, apps: ['notes', 'todo'], stopped: ['work'] });
		mountPage(Page);
		await settle();
		expect(text()).toContain('Stopped apps');
		expect(text()).toContain(
			'“Resume” syncs an app with the main again at once; a restart of this server resumes them all.',
		);
		expect(resumeButton('todo')?.textContent?.trim()).toBe('Resume');
		expect(resumeButton('work')).not.toBeNull();

		resumeButton('todo')?.focus();
		resumeButton('todo')?.click();
		await settle();
		expect(resume).toHaveBeenCalledWith('todo');
		expect(document.querySelector('[role="dialog"]')).toBeNull();
		expect(text()).toContain('todo syncs with the main again.');
		expect(resumeButton('todo')).toBeNull();
		expect(resumeButton('work')).not.toBeNull();
		// The button went with its row.
		expect(document.activeElement?.id).toBe('upstream-h');
	});

	it('says that a resumed app syncs once the main is in reach', async () => {
		upstream.mockResolvedValue({ ...linked, connected: false, stopped: ['todo'] });
		vi.spyOn(serverApi, 'resume').mockResolvedValue({ ...linked, connected: false });
		mountPage(Page);
		await settle();
		resumeButton('todo')?.click();
		await settle();
		expect(text()).toContain('todo syncs with the main again as soon as the main is in reach.');
		expect(text()).not.toContain('Stopped apps');
	});

	it('says why a resume failed, and shows the list as it is now', async () => {
		upstream.mockResolvedValue({ ...linked, stopped: ['todo'] });
		vi.spyOn(serverApi, 'resume').mockRejectedValue(
			new ApiError('notStopped', 'the write guard has not stopped this app'),
		);
		mountPage(Page);
		await settle();
		upstream.mockResolvedValue(linked); // resumed meanwhile, by a restart
		resumeButton('todo')?.click();
		await settle();
		expect(text()).toContain(
			'That app is not stopped any more. (the write guard has not stopped this app)',
		);
		expect(text()).not.toContain('Stopped apps');
		expect(document.activeElement?.id).toBe('upstream-h');
	});

	it('names the halt when the link halted before the press', async () => {
		upstream.mockResolvedValue({ ...linked, stopped: ['todo'] });
		vi.spyOn(serverApi, 'resume').mockRejectedValue(
			new ApiError('upstream', 'removed (badToken)', 'badToken'),
		);
		mountPage(Page);
		await settle();
		resumeButton('todo')?.click();
		await settle();
		expect(text()).toContain('The main removed this server. Unlink it;');
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
