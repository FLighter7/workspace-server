import { timingSafeEqual } from 'node:crypto';
import cors from '@fastify/cors';
import Fastify, { type FastifyInstance, type FastifyReply, type FastifyRequest } from 'fastify';
import { DocumentStore, PreconditionFailedError } from './DocumentStore.ts';
import { InvalidNameError } from './names.ts';

export interface ServerOptions {
	store: DocumentStore;
	token: string;
	/** Exact origins allowed to call in. A browser enforces this, not us. */
	allowedOrigins: string[];
	maxBodyBytes: number;
	logger?: boolean;
	/** Certificate and key contents. Omitted in tests, which never bind a port. */
	https?: { cert: Buffer; key: Buffer };
}

interface DocumentParams {
	app: string;
	doc: string;
}

const NAME = 'workspace-server';

/**
 * Built separately from listening so tests can drive it through `inject`
 * without a port, a certificate, or a teardown race.
 */
export function createServer(options: ServerOptions): FastifyInstance {
	const app = Fastify({
		logger: options.logger ?? false,
		bodyLimit: options.maxBodyBytes,
		...(options.https ? { https: options.https } : {}),
	}) as FastifyInstance;

	/**
	 * Bodies are kept as the exact bytes the client sent rather than parsed
	 * and re-serialized. The etag is a hash of what is on disk, so a body
	 * that came back differently spaced than it went in would change etag
	 * without anyone editing it, and every client would see a phantom
	 * conflict. Validity is still checked — a document that is not JSON is
	 * refused, it is just never rewritten.
	 */
	app.addContentTypeParser(
		'application/json',
		{ parseAs: 'string' },
		(_request, body: string, done) => {
			try {
				JSON.parse(body);
				done(null, body);
			} catch {
				done(badJson(), undefined);
			}
		},
	);

	void app.register(cors, {
		origin: options.allowedOrigins,
		methods: ['GET', 'PUT', 'OPTIONS'],
		allowedHeaders: ['Authorization', 'Content-Type', 'If-Match', 'If-None-Match'],
		exposedHeaders: ['ETag'],
		maxAge: 600,
	});

	/**
	 * The browser asks before letting a public page reach a private address.
	 * Granting it here is necessary but not sufficient: current Chrome and
	 * Edge also require the user to allow local network access for the site,
	 * through a prompt no header can answer on their behalf.
	 */
	app.addHook('onSend', async (request, reply) => {
		if (request.headers['access-control-request-private-network'] === 'true') {
			void reply.header('Access-Control-Allow-Private-Network', 'true');
		}
	});

	app.get('/api/health', async () => ({ name: NAME, version: 1 }));

	app.get<{ Params: DocumentParams }>('/api/apps/:app/docs/:doc', async (request, reply) => {
		if (!authorize(request, reply, options.token)) {
			return reply;
		}

		const stored = await options.store.read(request.params.app, request.params.doc);
		if (stored === null) {
			return reply.code(404).send({ error: 'no such document' });
		}

		return reply
			.header('ETag', stored.etag)
			.header('Cache-Control', 'no-store')
			.type('application/json')
			.send(stored.body);
	});

	app.put<{ Params: DocumentParams; Body: string }>(
		'/api/apps/:app/docs/:doc',
		async (request, reply) => {
			if (!authorize(request, reply, options.token)) {
				return reply;
			}

			const precondition = readPrecondition(request);
			if (precondition === undefined) {
				return reply
					.code(428)
					.send({ error: 'send If-Match with the etag you read, or If-None-Match: * to create' });
			}

			const existed = (await options.store.read(request.params.app, request.params.doc)) !== null;
			const { etag } = await options.store.write(
				request.params.app,
				request.params.doc,
				request.body,
				precondition,
			);

			return reply
				.code(existed ? 200 : 201)
				.header('ETag', etag)
				.send({ etag });
		},
	);

	app.setErrorHandler((error: unknown, _request, reply) => {
		if (error instanceof InvalidNameError) {
			return reply.code(400).send({ error: error.message });
		}

		if (error instanceof PreconditionFailedError) {
			return reply.code(412).send({ error: error.message });
		}

		const status = statusOf(error);
		return reply.code(status).send({ error: status === 500 ? 'internal error' : messageOf(error) });
	});

	return app;
}

/**
 * `null` means If-None-Match: * (create), a string means If-Match (update),
 * `undefined` means neither was sent and the request is refused. An
 * unconditional write is not offered at all: every client either read an
 * etag or got a 404, so every client can say which case it is in.
 */
function readPrecondition(request: FastifyRequest): string | null | undefined {
	const ifMatch = request.headers['if-match'];
	const ifNoneMatch = request.headers['if-none-match'];

	if (typeof ifNoneMatch === 'string' && ifNoneMatch.trim() === '*') {
		return null;
	}

	if (typeof ifMatch === 'string' && ifMatch.trim() !== '') {
		return ifMatch.trim();
	}

	return undefined;
}

function authorize(request: FastifyRequest, reply: FastifyReply, token: string): boolean {
	const header = request.headers.authorization;

	if (typeof header !== 'string' || !header.startsWith('Bearer ')) {
		void reply.code(401).send({ error: 'a bearer token is required' });
		return false;
	}

	if (!sameSecret(header.slice('Bearer '.length), token)) {
		void reply.code(401).send({ error: 'that token is not valid here' });
		return false;
	}

	return true;
}

/**
 * Constant-time within a length, and length-safe outside it: comparing
 * buffers of different sizes throws rather than returning false, so the
 * length check has to come first and is itself not secret — a token's
 * length leaks from nothing an attacker can measure here.
 */
function sameSecret(candidate: string, expected: string): boolean {
	const a = Buffer.from(candidate, 'utf8');
	const b = Buffer.from(expected, 'utf8');

	return a.length === b.length && timingSafeEqual(a, b);
}

function statusOf(error: unknown): number {
	if (typeof error === 'object' && error !== null && 'statusCode' in error) {
		const status = (error as { statusCode?: unknown }).statusCode;
		if (typeof status === 'number' && status >= 400 && status <= 599) {
			return status;
		}
	}

	return 500;
}

function messageOf(error: unknown): string {
	return error instanceof Error ? error.message : 'request failed';
}

function badJson(): Error & { statusCode?: number } {
	const error: Error & { statusCode?: number } = new Error('the body is not valid JSON');
	error.statusCode = 400;
	return error;
}
