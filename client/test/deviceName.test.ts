import { describe, expect, it } from 'vitest';
import { deviceName } from '../src/deviceName.js';

const IPHONE_SAFARI =
	'Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.6.1 Mobile/15E148 Safari/604.1';
const IPHONE_CHROME =
	'Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/157.0.8103.37 Mobile/15E148 Safari/604.1';
const MAC_CHROME =
	'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36';
const WINDOWS_EDGE =
	'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0';

describe('deviceName', () => {
	it('names an iPhone with Safari', () => {
		expect(deviceName(IPHONE_SAFARI, false)).toBe('iPhone Safari');
		expect(deviceName(IPHONE_SAFARI, true)).toBe('iPhone Safari (app)');
	});

	it('names an iPhone with Chrome', () => {
		expect(deviceName(IPHONE_CHROME, false)).toBe('iPhone Chrome');
	});

	it('names a desktop Chrome on macOS', () => {
		expect(deviceName(MAC_CHROME, false)).toBe('Mac Chrome');
	});

	it('names Edge on Windows', () => {
		expect(deviceName(WINDOWS_EDGE, false)).toBe('Windows Edge');
	});

	it('falls back for an agent it does not know', () => {
		expect(deviceName('curl/8.0', false)).toBe('Device Browser');
	});
});
