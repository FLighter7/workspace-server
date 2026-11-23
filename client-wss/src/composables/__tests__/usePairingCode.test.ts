import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Pairing } from '@/api/types';
import { createPairingCode } from '../usePairingCode';

const pairing = (code: string): Pairing => ({
	code,
	expiresAt: new Date(Date.now() + 600_000).toISOString(),
	qr: 'data:image/png;base64,',
});

afterEach(() => {
	vi.useRealTimers();
});

describe('createPairingCode', () => {
	it('issues a code for the machine asked, and again for the same one by default', async () => {
		const request = vi.fn(async (machine: string) => pairing(`code-${machine}`));
		const code = createPairingCode(request);
		await code.issue('Machine000000001');
		expect(request).toHaveBeenLastCalledWith('Machine000000001');
		expect(code.state.machine).toBe('Machine000000001');
		expect(code.state.text).toBe('code-Machine000000001');
		await code.issue();
		expect(request).toHaveBeenLastCalledWith('Machine000000001');
		code.drop();
		expect(code.state.machine).toBe('');
	});

	it('issues the last machine asked for while a code was in flight', async () => {
		let release: () => void = () => {};
		const request = vi.fn(
			(machine: string) =>
				new Promise<Pairing>((resolve) => {
					release = () => resolve(pairing(`code-${machine}`));
				}),
		);
		const code = createPairingCode(request);
		const first = code.issue('');
		void code.issue('Machine000000001');
		void code.issue('Machine000000002');
		release();
		await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
		release();
		await first;
		expect(request.mock.calls.map(([machine]) => machine)).toEqual(['', 'Machine000000002']);
		expect(code.state.machine).toBe('Machine000000002');
		code.drop();
	});

	it('asks nothing more for the same machine while one is in flight', async () => {
		const request = vi.fn(async (machine: string) => pairing(machine));
		const code = createPairingCode(request);
		await Promise.all([code.issue('M'), code.issue('M')]);
		expect(request).toHaveBeenCalledTimes(1);
		code.drop();
	});
});
