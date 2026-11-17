import { describe, expect, it } from 'vitest';
import { keepIdChars } from '../ids';

describe('keepIdChars', () => {
	it('keeps lowercase Latin letters, digits and hyphens, and drops the rest', () => {
		expect(keepIdChars('my-app-2')).toBe('my-app-2');
		expect(keepIdChars('My_App 2!')).toBe('y' + 'pp2');
		expect(keepIdChars('Приложение')).toBe('');
		expect(keepIdChars('')).toBe('');
	});
});
