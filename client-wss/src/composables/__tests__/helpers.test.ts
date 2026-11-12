// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/api/client';
import { errorKey, errorMessage } from '../messages';
import { haltedKey, upstreamError } from '../upstream';
import { formatLeft } from '../usePairingCode';
import { formatBytes } from '../useFormat';
import { startPolling } from '../usePolling';

describe('formatBytes', () => {
	it.each([
		[0, 'en', '0 bytes'],
		[999, 'en', '999 bytes'],
		[1500, 'en', '1.5 kB'],
		[67_108_864, 'en', '67.1 MB'],
		[2_000_000_000_000, 'en', '2,000 GB'],
	])('%d in %s is %s', (bytes, lang, want) => {
		expect(formatBytes(bytes, lang)).toBe(want);
	});

	it('writes the unit in the language', () => {
		expect(formatBytes(1500, 'ru')).toMatch(/^1,5\sкБ$/);
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
