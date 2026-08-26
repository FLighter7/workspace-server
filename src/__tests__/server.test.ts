import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DocumentStore } from '../DocumentStore.ts';
import { createServer } from '../server.ts';

const TOKEN = 'test-token-value';
const ORIGIN = 'https://flighter7.github.io';
const DOC = '/api/apps/todo/docs/data';
const BODY = { dreams: [], goals: [], tasks: [] };

let app: FastifyInstance;

beforeEach(async () => {
	const root = await mkdtemp(join(tmpdir(), 'workspace-server-http-'));
	app = createServer({
		store: new DocumentStore(root),
		token: TOKEN,
		allowedOrigins: [ORIGIN],
		maxBodyBytes: 1024,
	});
	await app.ready();
});

afterEach(async () => {
	await app.close();
});

function authed(extra: Record<string, string> = {}): Record<string, string> {
	return { authorization: `Bearer ${TOKEN}`, ...extra };
}

async function create(body: unknown = BODY) {
	return app.inject({
		method: 'PUT',
		url: DOC,
		headers: authed({ 'content-type': 'application/json', 'if-none-match': '*' }),
		payload: JSON.stringify(body),
	});
}

describe('health', () => {
	it('answers without a token, so a client can probe before it is paired', async () => {
		const response = await app.inject({ method: 'GET', url: '/api/health' });

		expect(response.statusCode).toBe(200);
		expect(response.json()).toMatchObject({ name: 'workspace-server' });
	});

	it('does not leak the token', async () => {
		const response = await app.inject({ method: 'GET', url: '/api/health' });

		expect(response.body).not.toContain(TOKEN);
	});
});

describe('authorization', () => {
	it('refuses a document request with no token', async () => {
		const response = await app.inject({ method: 'GET', url: DOC });

		expect(response.statusCode).toBe(401);
	});

	it('refuses a wrong token', async () => {
		const response = await app.inject({
			method: 'GET',
			url: DOC,
			headers: { authorization: 'Bearer nope' },
		});

		expect(response.statusCode).toBe(401);
	});

	it('refuses a token of the right length but wrong content', async () => {
		const response = await app.inject({
			method: 'GET',
			url: DOC,
			headers: { authorization: `Bearer ${'x'.repeat(TOKEN.length)}` },
		});

		expect(response.statusCode).toBe(401);
	});

	it('refuses a scheme other than Bearer', async () => {
		const response = await app.inject({
			method: 'GET',
			url: DOC,
			headers: { authorization: `Basic ${TOKEN}` },
		});

		expect(response.statusCode).toBe(401);
	});
});

describe('reading a document', () => {
	it('reports 404 for a document that was never written', async () => {
		const response = await app.inject({ method: 'GET', url: DOC, headers: authed() });

		expect(response.statusCode).toBe(404);
	});

	it('returns the stored body with an etag', async () => {
		await create();

		const response = await app.inject({ method: 'GET', url: DOC, headers: authed() });

		expect(response.statusCode).toBe(200);
		expect(response.json()).toEqual(BODY);
		expect(response.headers.etag).toMatch(/^"[0-9a-f]{64}"$/);
	});

	it('tells caches not to keep it — a stale body carries a stale etag', async () => {
		await create();

		const response = await app.inject({ method: 'GET', url: DOC, headers: authed() });

		expect(response.headers['cache-control']).toContain('no-store');
	});

	it('returns the same etag the write reported', async () => {
		const written = await create();

		const response = await app.inject({ method: 'GET', url: DOC, headers: authed() });

		expect(response.headers.etag).toBe(written.headers.etag);
	});
});

describe('writing a document', () => {
	it('creates a document under If-None-Match: *', async () => {
		const response = await create();

		expect(response.statusCode).toBe(201);
		expect(response.headers.etag).toMatch(/^"[0-9a-f]{64}"$/);
	});

	it('refuses to create twice', async () => {
		await create();

		expect((await create()).statusCode).toBe(412);
	});

	it('updates under a matching If-Match', async () => {
		const created = await create();

		const response = await app.inject({
			method: 'PUT',
			url: DOC,
			headers: authed({
				'content-type': 'application/json',
				'if-match': created.headers.etag as string,
			}),
			payload: JSON.stringify({ dreams: [], goals: [], tasks: [{ id: 'x' }] }),
		});

		expect(response.statusCode).toBe(200);
		expect(response.headers.etag).not.toBe(created.headers.etag);
	});

	it('refuses an update under a stale If-Match', async () => {
		await create();

		const response = await app.inject({
			method: 'PUT',
			url: DOC,
			headers: authed({ 'content-type': 'application/json', 'if-match': '"stale"' }),
			payload: JSON.stringify(BODY),
		});

		expect(response.statusCode).toBe(412);
	});

	it('requires a precondition, so nothing can be clobbered by accident', async () => {
		const response = await app.inject({
			method: 'PUT',
			url: DOC,
			headers: authed({ 'content-type': 'application/json' }),
			payload: JSON.stringify(BODY),
		});

		expect(response.statusCode).toBe(428);
	});

	it('stores the bytes it was sent, so the etag stays stable across a round trip', async () => {
		const spaced = '{ "dreams" : [ ] , "goals" : [ ] , "tasks" : [ ] }';
		await app.inject({
			method: 'PUT',
			url: DOC,
			headers: authed({ 'content-type': 'application/json', 'if-none-match': '*' }),
			payload: spaced,
		});

		const response = await app.inject({ method: 'GET', url: DOC, headers: authed() });

		expect(response.body).toBe(spaced);
	});

	it('refuses a body that is not JSON', async () => {
		const response = await app.inject({
			method: 'PUT',
			url: DOC,
			headers: authed({ 'content-type': 'application/json', 'if-none-match': '*' }),
			payload: 'not json at all',
		});

		expect(response.statusCode).toBe(400);
	});

	it('refuses a body past the size limit', async () => {
		const response = await app.inject({
			method: 'PUT',
			url: DOC,
			headers: authed({ 'content-type': 'application/json', 'if-none-match': '*' }),
			payload: JSON.stringify({ padding: 'x'.repeat(2048) }),
		});

		expect(response.statusCode).toBe(413);
	});
});

describe('name safety', () => {
	it('refuses a traversal in the app segment', async () => {
		const response = await app.inject({
			method: 'GET',
			url: '/api/apps/..%2F..%2Fetc/docs/data',
			headers: authed(),
		});

		expect([400, 404]).toContain(response.statusCode);
	});

	it('refuses an uppercase app name', async () => {
		const response = await app.inject({
			method: 'GET',
			url: '/api/apps/Todo/docs/data',
			headers: authed(),
		});

		expect(response.statusCode).toBe(400);
	});

	it('refuses a dot in the document name', async () => {
		const response = await app.inject({
			method: 'GET',
			url: '/api/apps/todo/docs/data.json',
			headers: authed(),
		});

		expect(response.statusCode).toBe(400);
	});
});

describe('CORS', () => {
	it('allows an origin on the list', async () => {
		await create();

		const response = await app.inject({
			method: 'GET',
			url: DOC,
			headers: authed({ origin: ORIGIN }),
		});

		expect(response.headers['access-control-allow-origin']).toBe(ORIGIN);
	});

	it('exposes ETag, without which a client cannot read it at all', async () => {
		await create();

		const response = await app.inject({
			method: 'GET',
			url: DOC,
			headers: authed({ origin: ORIGIN }),
		});

		expect(String(response.headers['access-control-expose-headers']).toLowerCase()).toContain(
			'etag',
		);
	});

	it('answers a preflight with the headers a conditional PUT needs', async () => {
		const response = await app.inject({
			method: 'OPTIONS',
			url: DOC,
			headers: {
				origin: ORIGIN,
				'access-control-request-method': 'PUT',
				'access-control-request-headers': 'authorization,content-type,if-match',
			},
		});

		expect(response.statusCode).toBeLessThan(300);

		const allowed = String(response.headers['access-control-allow-headers']).toLowerCase();
		expect(allowed).toContain('authorization');
		expect(allowed).toContain('if-match');
		expect(allowed).toContain('if-none-match');
		expect(String(response.headers['access-control-allow-methods'])).toContain('PUT');
	});

	it('grants the private-network preflight browsers ask for from a public page', async () => {
		const response = await app.inject({
			method: 'OPTIONS',
			url: DOC,
			headers: {
				origin: ORIGIN,
				'access-control-request-method': 'GET',
				'access-control-request-private-network': 'true',
			},
		});

		expect(response.headers['access-control-allow-private-network']).toBe('true');
	});

	it('does not hand its origin header to a site that is not on the list', async () => {
		const response = await app.inject({
			method: 'GET',
			url: DOC,
			headers: authed({ origin: 'https://evil.example' }),
		});

		expect(response.headers['access-control-allow-origin']).not.toBe('https://evil.example');
	});
});
