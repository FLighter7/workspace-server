// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { serverApi } from '@/api/client';
import type { DataDirCheck, DataDirVerdict, Status } from '@/api/types';
import { mountPage, settle } from '@/testing/mountPage';
import ServerSection from '../ServerSection.vue';

const status: Status = {
	name: 'Home',
	serverId: 'srv1',
	mdnsName: 'abc.local',
	mdnsRunning: true,
	listening: true,
	addresses: ['192.168.1.2'],
	udpPort: 47800,
	httpPort: 47801,
	program: 'C:\\ws\\workspace-server.exe',
	serverDir: 'C:\\ws',
	dataDir: 'C:\\ws',
	mainServer: true,
	lastError: '',
	role: 'main',
};

beforeEach(() => {
	vi.spyOn(serverApi, 'status').mockResolvedValue(status);
	vi.spyOn(serverApi, 'upstream').mockResolvedValue({
		linked: false,
		connected: false,
		halted: false,
		apps: [],
		subordinates: 0,
	});
});

afterEach(() => {
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

const button = (label: string): HTMLButtonElement =>
	[...document.querySelectorAll('button')].find(
		(b) => b.textContent?.trim() === label,
	) as HTMLButtonElement;

function check(verdict: DataDirVerdict, extra: Partial<DataDirCheck> = {}): DataDirCheck {
	return { path: 'D:\\data', verdict, message: '', current: [], target: [], ...extra };
}

/** Types the folder and presses Change. */
async function changeTo(result: DataDirCheck): Promise<void> {
	vi.spyOn(serverApi, 'checkDataDir').mockResolvedValue(result);
	const field = [...document.querySelectorAll('input')].find(
		(input) => input.value === status.dataDir,
	) as HTMLInputElement;
	field.value = 'D:\\data';
	field.dispatchEvent(new Event('input'));
	await settle();
	button('Change').closest('form')?.dispatchEvent(new Event('submit'));
	await settle();
}

describe('ServerSection: changing the data folder', () => {
	it('shows the server facts and fills the form', async () => {
		mountPage(ServerSection);
		await settle();
		const text = document.body.textContent ?? '';
		expect(text).toContain('srv1');
		expect(text).toContain('192.168.1.2');
		expect(text).toContain('Published on the local network');
		expect([...document.querySelectorAll('input')].map((i) => i.value)).toEqual([
			'Home',
			'47800',
			'47801',
			'C:\\ws',
		]);
	});

	it('refuses a folder that already holds data, without a question', async () => {
		const setDataDir = vi.spyOn(serverApi, 'setDataDir');
		mountPage(ServerSection);
		await settle();
		await changeTo(check('conflict'));

		expect(document.body.textContent).toContain(
			'This folder already holds data of a workspace-server.',
		);
		expect(setDataDir).not.toHaveBeenCalled();
		expect(document.querySelector('[role="dialog"]')).toBeNull();
	});

	it("says why a folder cannot be used, with the server's detail", async () => {
		mountPage(ServerSection);
		await settle();
		await changeTo(check('unavailable', { message: 'access denied' }));
		expect(document.body.textContent).toContain('This folder cannot be used. (access denied)');
	});

	it('asks before it adopts the data in the folder, listing it', async () => {
		const setDataDir = vi.spyOn(serverApi, 'setDataDir');
		mountPage(ServerSection);
		await settle();
		await changeTo(check('adopt', { target: [{ id: 'todo', size: 1500, versions: 2 }] }));

		const dialog = document.querySelector('[role="dialog"]');
		expect(dialog?.textContent).toContain('Use the data in this folder?');
		expect(dialog?.textContent).toContain('todo');
		expect(dialog?.textContent).toContain('1.5 kB');
		expect(setDataDir).not.toHaveBeenCalled();
	});

	it('moves without a question when there is nothing to move', async () => {
		const setDataDir = vi.spyOn(serverApi, 'setDataDir').mockResolvedValue(null);
		mountPage(ServerSection);
		await settle();
		await changeTo(check('create'));

		expect(setDataDir).toHaveBeenCalledWith('D:\\data', 'create');
		expect(document.querySelector('[role="dialog"]')).toBeNull();
	});

	it('asks before it moves the current data', async () => {
		const setDataDir = vi.spyOn(serverApi, 'setDataDir');
		mountPage(ServerSection);
		await settle();
		await changeTo(check('move', { current: [{ id: 'todo', size: 10, versions: 0 }] }));

		expect(document.querySelector('[role="dialog"]')?.textContent).toContain('Move the data?');
		expect(setDataDir).not.toHaveBeenCalled();
	});
});
