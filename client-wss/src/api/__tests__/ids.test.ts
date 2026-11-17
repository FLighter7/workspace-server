import { describe, expect, it } from 'vitest';
import { keepIdChars } from '../ids';

describe('keepIdChars', () => {
	it('lowercases, keeps Latin letters, digits and hyphens, and drops the rest', () => {
		expect(keepIdChars('my-app-2')).toBe('my-app-2');
		expect(keepIdChars('My_App 2!')).toBe('myapp2');
		expect(keepIdChars('Приложение')).toBe('');
		expect(keepIdChars('')).toBe('');
	});
});
