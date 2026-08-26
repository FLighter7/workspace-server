import { mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { beforeEach, describe, expect, it } from 'vitest';
import { ConfigError, loadConfig, resolveToken } from '../config.ts';

let root: string;

beforeEach(async () => {
	root = await mkdtemp(join(tmpdir(), 'workspace-server-config-'));
});

function env(overrides: Record<string, string | undefined> = {}) {
	return { DATA_ROOT: root, ...overrides };
}

describe('loadConfig', () => {
	it('refuses to start without a data root — there is nowhere to put anything', () => {
		expect(() => loadConfig({})).toThrow(ConfigError);
	});

	it('defaults the port to 8443', () => {
		expect(loadConfig(env()).port).toBe(8443);
	});

	it('takes a port from the environment', () => {
		expect(loadConfig(env({ PORT: '9000' })).port).toBe(9000);
	});

	it('refuses a port that is not a number', () => {
		expect(() => loadConfig(env({ PORT: 'soon' }))).toThrow(ConfigError);
	});

	it('refuses a port outside the usable range', () => {
		expect(() => loadConfig(env({ PORT: '70000' }))).toThrow(ConfigError);
	});

	it('splits allowed origins on commas and trims them', () => {
		const config = loadConfig(env({ ALLOWED_ORIGINS: 'https://a.example , https://b.example' }));

		expect(config.allowedOrigins.slice(0, 2)).toEqual(['https://a.example', 'https://b.example']);
	});

	it('always allows the dev and preview origins, so local work needs no setup', () => {
		expect(loadConfig(env()).allowedOrigins).toEqual(
			expect.arrayContaining(['http://localhost:5173', 'http://localhost:4173']),
		);
	});

	it('does not duplicate an origin that is also a default', () => {
		const config = loadConfig(env({ ALLOWED_ORIGINS: 'http://localhost:5173' }));

		expect(config.allowedOrigins.filter((o) => o === 'http://localhost:5173')).toHaveLength(1);
	});

	it('defaults the body limit to 5 MB', () => {
		expect(loadConfig(env()).maxBodyBytes).toBe(5 * 1024 * 1024);
	});

	it('reports no TLS when neither certificate nor key is given', () => {
		expect(loadConfig(env()).tls).toBeNull();
	});

	it('reports TLS when both are given', () => {
		expect(loadConfig(env({ TLS_CERT: 'c.pem', TLS_KEY: 'k.pem' })).tls).toEqual({
			cert: 'c.pem',
			key: 'k.pem',
		});
	});

	it('refuses half a TLS configuration rather than silently serving plain HTTP', () => {
		expect(() => loadConfig(env({ TLS_CERT: 'c.pem' }))).toThrow(ConfigError);
		expect(() => loadConfig(env({ TLS_KEY: 'k.pem' }))).toThrow(ConfigError);
	});
});

describe('resolveToken', () => {
	it('takes the token from the environment when one is set', async () => {
		expect(await resolveToken({ TOKEN: 'from-env' }, root)).toBe('from-env');
	});

	it('generates a token on first run', async () => {
		const token = await resolveToken({}, root);

		expect(token).toMatch(/^[A-Za-z0-9_-]{32,}$/);
	});

	it('remembers the generated token across restarts', async () => {
		const first = await resolveToken({}, root);

		expect(await resolveToken({}, root)).toBe(first);
	});

	it('writes the generated token where it can be read back', async () => {
		const token = await resolveToken({}, root);

		expect((await readFile(join(root, '.token'), 'utf8')).trim()).toBe(token);
	});

	it('tolerates trailing whitespace in a hand-edited token file', async () => {
		await writeFile(join(root, '.token'), '  hand-written  \n', 'utf8');

		expect(await resolveToken({}, root)).toBe('hand-written');
	});

	it('regenerates rather than trusting an empty token file', async () => {
		await writeFile(join(root, '.token'), '   \n', 'utf8');

		expect(await resolveToken({}, root)).toMatch(/^[A-Za-z0-9_-]{32,}$/);
	});

	it('prefers the environment over a stored token, so rotation is one restart', async () => {
		await resolveToken({}, root);

		expect(await resolveToken({ TOKEN: 'rotated' }, root)).toBe('rotated');
	});
});
