import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, request, serverApi } from '../client';

function respond(status: number, body?: unknown, statusText = ''): void {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () =>
			body === undefined
				? new Response(null, { status, statusText })
				: new Response(typeof body === 'string' ? body : JSON.stringify(body), {
						status,
						statusText,
					}),
		),
	);
}

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('request', () => {
	it('marks every request and sends a body as JSON', async () => {
		respond(204);
		await serverApi.setConfig({ name: 'Home' });

		const [path, init] = vi.mocked(fetch).mock.calls[0];
		expect(path).toBe('/api/config');
		expect(init?.method).toBe('PUT');
		expect(init?.cache).toBe('no-store');
		expect(init?.headers).toEqual({
			'X-Workspace-Server': '1',
			'Content-Type': 'application/json',
		});
		expect(init?.body).toBe('{"name":"Home"}');
	});

	it('encodes ids and paths', async () => {
		respond(200, { path: 'x', verdict: 'same', message: '', current: [], target: [] });
		await serverApi.checkDataDir('C:\\Data & more');
		expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/data-dir?path=C%3A%5CData%20%26%20more');

		respond(204);
		await serverApi.removeDevice('a/b');
		expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/devices/a%2Fb');
	});

	it('resolves 201 and 204 to null, anything else to its JSON', async () => {
		respond(201);
		expect(await request('POST', '/api/apps', {})).toBeNull();
		respond(204);
		expect(await request('DELETE', '/api/data')).toBeNull();
		respond(202, { restarting: true });
		expect(await request('PUT', '/api/config', {})).toEqual({ restarting: true });
	});

	it("rejects with the server's code and message", async () => {
		respond(409, { error: 'dataExists', message: 'the folder already holds data' });
		await expect(request('PUT', '/api/config')).rejects.toEqual(
			new ApiError('dataExists', 'the folder already holds data'),
		);
	});

	it("keeps why the upstream server refused, and the routes' paths", async () => {
		respond(502, { error: 'upstream', message: 'used (badSecret)', reason: 'badSecret' });
		const error = await serverApi.link('wsrv1.x').catch((e: unknown) => e);
		expect(error).toMatchObject({ code: 'upstream', reason: 'badSecret' });
		const [path, init] = vi.mocked(fetch).mock.calls[0];
		expect([path, init?.method, init?.body]).toEqual([
			'/api/upstream',
			'POST',
			'{"code":"wsrv1.x"}',
		]);

		respond(200, { forgotten: true });
		await serverApi.unlink();
		expect(vi.mocked(fetch).mock.calls[0].slice(0, 1)).toEqual(['/api/upstream']);
		expect(vi.mocked(fetch).mock.calls[0][1]?.method).toBe('DELETE');
		respond(200, { code: 'c', expiresAt: '', qr: '' });
		await serverApi.invite();
		expect(vi.mocked(fetch).mock.calls[0][0]).toBe('/api/upstream/invite');
	});

	it('reads a failure without JSON as internal, with the status text', async () => {
		respond(502, 'Bad gateway', 'Bad Gateway');
		const error = await request('GET', '/api/status').catch((e: unknown) => e);
		expect(error).toBeInstanceOf(ApiError);
		expect(error).toMatchObject({ code: 'internal', message: 'Bad Gateway' });
	});

	it('rejects with "network" when the server cannot be reached', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(async () => {
				throw new TypeError('Failed to fetch');
			}),
		);
		await expect(request('GET', '/api/status')).rejects.toMatchObject({ code: 'network' });
	});
});
