// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/api/client';
import { errorKey, errorMessage } from '../messages';
import { haltedKey, upstreamError } from '../upstream';
import { formatLeft } from '../usePairingCode';
import { formatBytes, formatWhen } from '../useFormat';
import { startPolling } from '../usePolling';

describe('formatBytes', () => {
	it.each([
		[0, 'en', '0 bytes'],
		[1023, 'en', '1,023 bytes'],
		[1536, 'en', '1.5 kB'],
		[52_428_800, 'en', '50 MB'],
		[2_147_483_648, 'en', '2 GB'],
		[1_099_511_627_776, 'en', '1 TB'],
		[3 * 1_099_511_627_776, 'en', '3 TB'],
	])('%d in %s is %s', (bytes, lang, want) => {
		expect(formatBytes(bytes, lang)).toBe(want);
	});

	it('writes the unit in the language', () => {
		expect(formatBytes(1536, 'ru')).toMatch(/^1,5\sкБ$/);
	});
});

describe('formatLeft', () => {
	it.each([
		[600, '10:00'],
		[61, '1:01'],
		[9, '0:09'],
	])('%d seconds is %s', (seconds, want) => {
		expect(formatLeft(seconds)).toBe(want);
	});
});

describe('errorMessage', () => {
	it("maps the server's codes to the page's texts, and keeps its detail", () => {
		expect(errorMessage(new ApiError('exists', 'app "todo" exists'))).toEqual({
			key: 'errExists',
			detail: 'app "todo" exists',
			error: true,
		});
		expect(errorKey(new ApiError('changed', ''))).toBe('errDataChanged');
		expect(errorKey(new ApiError('network', ''))).toBe('errNetwork');
		expect(errorKey(new ApiError('followsMain', ''))).toBe('errFollowsMain');
	});

	it('reads an unknown code, or no ApiError at all, as internal', () => {
		expect(errorKey(new ApiError('teapot', ''))).toBe('errInternal');
		expect(errorKey(new TypeError('x'))).toBe('errInternal');
		expect(errorKey('boom')).toBe('errInternal');
	});
});

describe('startPolling', () => {
	afterEach(() => {
		vi.useRealTimers();
		vi.restoreAllMocks();
	});

	it('calls now, then each interval after the previous call, until stopped', async () => {
		vi.useFakeTimers();
		const fn = vi.fn(async () => {});
		const stop = startPolling(fn, 1000);
		await vi.advanceTimersByTimeAsync(0);
		expect(fn).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(2000);
		expect(fn).toHaveBeenCalledTimes(3);

		stop();
		await vi.advanceTimersByTimeAsync(5000);
		expect(fn).toHaveBeenCalledTimes(3);
	});

	it('skips the calls while the page is hidden', async () => {
		vi.useFakeTimers();
		const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
		const fn = vi.fn(async () => {});
		const stop = startPolling(fn, 1000);
		await vi.advanceTimersByTimeAsync(3000);
		expect(fn).not.toHaveBeenCalled();

		visibility.mockReturnValue('visible');
		await vi.advanceTimersByTimeAsync(1000);
		expect(fn).toHaveBeenCalledTimes(1);
		stop();
	});
});

describe('upstreamError', () => {
	it("maps the main's reasons and the API's codes, keeping the detail", () => {
		expect(upstreamError(new ApiError('upstream', 'gone', 'connectTimeout'))).toEqual({
			key: 'linkUnreachable',
			detail: 'gone',
			error: true,
		});
		expect(upstreamError(new ApiError('upstream', '', 'notMain')).key).toBe('linkNotMain');
		expect(upstreamError(new ApiError('hasSubordinates', '')).key).toBe('linkHasSubordinates');
		expect(upstreamError(new ApiError('invalid', '')).key).toBe('linkInvalidCode');
	});

	it("takes an action's own words for a reason, and falls back to the generic texts", () => {
		const unreachable = new ApiError('upstream', '', 'unreachable');
		expect(upstreamError(unreachable, { unreachable: 'inviteUnreachable' }).key).toBe(
			'inviteUnreachable',
		);
		expect(upstreamError(new ApiError('upstream', '', 'teapot')).key).toBe('errInternal');
		expect(upstreamError(new ApiError('network', '')).key).toBe('errNetwork');
	});

	it('says what a halted link means', () => {
		expect(haltedKey('badToken')).toBe('haltedRemoved');
		expect(haltedKey('protocol')).toBe('haltedTooOld');
		expect(haltedKey(undefined)).toBe('haltedOther');
	});
});

describe('formatWhen', () => {
	// Noon UTC: the same day in any zone a test runs in.
	const now = new Date('2026-11-22T12:00:00Z');

	it('leaves the year out while it is this one', () => {
		expect(formatWhen('2026-11-03T12:00:00Z', 'en', now)).toMatch(/^Nov 3, \d/);
		expect(formatWhen('2026-11-03T12:00:00Z', 'ru', now)).toMatch(/^3 нояб\., \d/);
	});

	it('names the year of another one', () => {
		expect(formatWhen('2025-11-03T12:00:00Z', 'en', now)).toMatch(/^Nov 3, 2025, \d/);
		expect(formatWhen('2025-11-03T12:00:00Z', 'ru', now)).toMatch(/^3 нояб\. 2025 г\., \d/);
	});
});
