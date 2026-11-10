import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ServerTarget } from '../src/PairingCode.js';
import { buildServerAnswer, randomIceUfrag } from '../src/serverAnswer.js';

const FINGERPRINT =
	'0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9';

const TARGET: ServerTarget = {
	password: 'abcdefghijklmnopqrstuvwx',
	fingerprint: FINGERPRINT,
	port: 47800,
	hosts: ['0b7c5a52-6a3e-4d2b-9f1e-7d2c3b4a5f60.local', '192.168.1.20'],
};

function offer(mid: string): string {
	return [
		'v=0',
		'o=- 1 2 IN IP4 127.0.0.1',
		's=-',
		't=0 0',
		`a=group:BUNDLE ${mid}`,
		'm=application 9 UDP/DTLS/SCTP webrtc-datachannel',
		'c=IN IP4 0.0.0.0',
		`a=mid:${mid}`,
		'',
	].join('\r\n');
}

describe('buildServerAnswer', () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it('builds the answer line by line', () => {
		vi.spyOn(Date, 'now').mockReturnValue(1762500000000);

		expect(buildServerAnswer(offer('0'), TARGET, 'AbCd1234')).toBe(
			[
				'v=0',
				'o=- 1762500000000 2 IN IP4 127.0.0.1',
				's=-',
				't=0 0',
				'a=group:BUNDLE 0',
				'a=ice-lite',
				'a=msid-semantic: WMS',
				'm=application 47800 UDP/DTLS/SCTP webrtc-datachannel',
				'c=IN IP4 0.0.0.0',
				'a=candidate:1 1 udp 2130706431 0b7c5a52-6a3e-4d2b-9f1e-7d2c3b4a5f60.local 47800 typ host',
				'a=candidate:2 1 udp 2130706430 192.168.1.20 47800 typ host',
				'a=end-of-candidates',
				'a=ice-ufrag:AbCd1234',
				'a=ice-pwd:abcdefghijklmnopqrstuvwx',
				`a=fingerprint:sha-256 ${FINGERPRINT}`,
				'a=setup:passive',
				'a=mid:0',
				'a=sctp-port:5000',
				'a=max-message-size:262144',
				'',
			].join('\r\n'),
		);
	});

	it('copies a non-zero mid', () => {
		const answer = buildServerAnswer(offer('data'), TARGET, 'AbCd1234');

		expect(answer).toContain('\r\na=group:BUNDLE data\r\n');
		expect(answer).toContain('\r\na=mid:data\r\n');
	});

	it('falls back to mid 0 when the offer has none', () => {
		const answer = buildServerAnswer('v=0\r\n', TARGET, 'AbCd1234');

		expect(answer).toContain('\r\na=mid:0\r\n');
	});
});

describe('randomIceUfrag', () => {
	it('returns 8 ICE characters, new every time', () => {
		const first = randomIceUfrag();
		const second = randomIceUfrag();

		expect(first).toMatch(/^[A-Za-z0-9]{8}$/);
		expect(second).toMatch(/^[A-Za-z0-9]{8}$/);
		expect(first).not.toBe(second);
	});
});
