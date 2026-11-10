import { describe, expect, it } from 'vitest';
import { parsePairingCode } from '../src/PairingCode.js';
import { WorkspaceError, type WorkspaceErrorCode } from '../src/WorkspaceError.js';

function base64url(bytes: Uint8Array): string {
	let binary = '';
	for (const byte of bytes) binary += String.fromCharCode(byte);

	return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

const FINGERPRINT_BYTES = Uint8Array.from({ length: 32 }, (_, i) => i);

const VALID = {
	v: 1,
	id: 'S1',
	n: 'Desk',
	p: 'a'.repeat(24),
	f: base64url(FINGERPRINT_BYTES),
	o: 47800,
	h: ['1f6bd314-65d5-4089-bdcc-71743996540d.local', '192.168.1.5'],
	s: 'secret',
};

function payload(fields: unknown): string {
	return base64url(new TextEncoder().encode(JSON.stringify(fields)));
}

function makeCode(fields: Record<string, unknown> = {}): string {
	return 'wsrv1.' + payload({ ...VALID, ...fields });
}

function codeOf(text: string): WorkspaceErrorCode | null {
	try {
		parsePairingCode(text);
		return null;
	} catch (error) {
		expect(error).toBeInstanceOf(WorkspaceError);
		return (error as WorkspaceError).code;
	}
}

describe('parsePairingCode', () => {
	it('parses a valid code', () => {
		expect(parsePairingCode(makeCode())).toEqual({
			serverId: 'S1',
			name: 'Desk',
			secret: 'secret',
			password: 'a'.repeat(24),
			fingerprint:
				'00:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F:10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F',
			port: 47800,
			hosts: ['1f6bd314-65d5-4089-bdcc-71743996540d.local', '192.168.1.5'],
		});
	});

	it('accepts stray whitespace around the code', () => {
		expect(parsePairingCode(`  ${makeCode()}\n`).serverId).toBe('S1');
		expect(parsePairingCode(`\t${makeCode()}\r\n`).fingerprint).toHaveLength(95);
	});

	it('refuses text that is not a code', () => {
		expect(codeOf('hello')).toBe('notPairingCode');
		expect(codeOf('1oc.abc')).toBe('notPairingCode');
		expect(codeOf('')).toBe('notPairingCode');
		expect(codeOf('wsrv1.')).toBe('notPairingCode');
	});

	it('refuses a newer version', () => {
		expect(codeOf('wsrv2.' + payload(VALID))).toBe('tooNew');
	});

	it('refuses a damaged code', () => {
		const full = makeCode();
		expect(codeOf(full.slice(0, full.length - 7))).toBe('damaged');
		expect(codeOf('wsrv1.' + base64url(new TextEncoder().encode('not json')))).toBe('damaged');
		expect(codeOf(makeCode({ f: base64url(FINGERPRINT_BYTES.slice(0, 31)) }))).toBe('damaged');
		expect(codeOf(makeCode({ o: 0 }))).toBe('damaged');
		expect(codeOf(makeCode({ o: 65536 }))).toBe('damaged');
		expect(codeOf(makeCode({ h: [] }))).toBe('damaged');
		expect(codeOf(makeCode({ h: [''] }))).toBe('damaged');
		expect(codeOf(makeCode({ p: 'a'.repeat(10) }))).toBe('damaged');
		expect(codeOf(makeCode({ v: 2 }))).toBe('damaged');
		expect(codeOf(makeCode({ s: '' }))).toBe('damaged');
		expect(codeOf(makeCode({ id: 7 }))).toBe('damaged');
		expect(codeOf('wsrv1.' + payload([1, 2]).replace(/=/g, ''))).toBe('damaged');
	});
});
