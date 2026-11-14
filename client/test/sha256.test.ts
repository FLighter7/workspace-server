import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { Sha256, etagOf } from '../src/sha256.js';

const nodeHex = (bytes: Uint8Array): string => createHash('sha256').update(bytes).digest('hex');

/** `size` bytes that are not all alike. */
function bytes(size: number): Uint8Array<ArrayBuffer> {
	return Uint8Array.from({ length: size }, (_, i) => (i * 31 + 7) % 256);
}

describe('Sha256', () => {
	it('matches the standard vectors', () => {
		const text = (s: string): Uint8Array => new TextEncoder().encode(s);
		expect(new Sha256().update(text('')).hex()).toBe(
			'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
		);
		expect(new Sha256().update(text('abc')).hex()).toBe(
			'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
		);
		expect(
			new Sha256().update(text('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq')).hex(),
		).toBe('248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1');
	});

	it('agrees with node’s at every length around a block, fed in any pieces', () => {
		for (let size = 0; size <= 200; size++) {
			const data = bytes(size);
			expect(new Sha256().update(data).hex()).toBe(nodeHex(data));
			const split = Math.floor(size / 3);
			expect(
				new Sha256()
					.update(data.subarray(0, split))
					.update(data.subarray(split, 2 * split))
					.update(data.subarray(2 * split))
					.hex(),
			).toBe(nodeHex(data));
		}
	});

	it('hashes a Blob a slice at a time, as the server names its etag', async () => {
		const data = bytes(3 * 1024 * 1024 + 17);

		await expect(etagOf(new Blob([data]))).resolves.toBe(nodeHex(data));
		await expect(etagOf(data)).resolves.toBe(nodeHex(data));
	});
});
