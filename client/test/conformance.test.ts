import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import {
	BUFFER_HIGH_BYTES,
	BUFFER_LOW_BYTES,
	ChannelFraming,
	MAX_MESSAGE_BYTES,
	type Incoming,
} from '../src/ChannelFraming.js';
import { PAIRING_CODE_VERSION, parsePairingCode, type ServerTarget } from '../src/PairingCode.js';
import { buildServerAnswer } from '../src/serverAnswer.js';
import {
	LIST_PAGE,
	PROTOCOL_VERSION,
	ServerClient,
	isStale,
	type DocumentKey,
	type Keep,
} from '../src/ServerClient.js';
import { SERVER_ERROR_CODES, WorkspaceError } from '../src/WorkspaceError.js';
import { FakeChannel } from '../src/testing/FakeChannel.js';
import { settle } from './fixtures.js';

/*
 * The shared vectors in testdata/conformance, played against this client.
 * The server's Go tests play the same files against its Session, so a
 * change on one side that the other does not follow fails a test.
 */

function vectors<T>(name: string): T {
	const url = new URL(`../../testdata/conformance/${name}`, import.meta.url);
	return JSON.parse(readFileSync(url, 'utf8')) as T;
}

type Json = Record<string, unknown>;

const utf8 = (text: string): Uint8Array => new TextEncoder().encode(text);
const text = (bytes: Uint8Array): string => new TextDecoder().decode(bytes);

describe('conformance: constants and codes', () => {
	it('matches the constants', () => {
		const c = vectors<{
			proto: number;
			pairingCodePrefix: string;
			chunkBytes: number;
			bufferHighBytes: number;
			bufferLowBytes: number;
			listPage: number;
		}>('constants.json');

		expect(PROTOCOL_VERSION).toBe(c.proto);
		expect(`wsrv${PAIRING_CODE_VERSION}.`).toBe(c.pairingCodePrefix);
		expect(MAX_MESSAGE_BYTES).toBe(c.chunkBytes);
		expect(BUFFER_HIGH_BYTES).toBe(c.bufferHighBytes);
		expect(BUFFER_LOW_BYTES).toBe(c.bufferLowBytes);
		expect(LIST_PAGE).toBe(c.listPage);
	});

	it('knows every server error code', () => {
		const { server } = vectors<{ server: string[] }>('error-codes.json');

		expect([...SERVER_ERROR_CODES].sort()).toEqual([...server].sort());
	});
});

describe('conformance: pairing codes', () => {
	const v = vectors<{
		valid: { name: string; code: string; parsed: Json }[];
		invalid: { name: string; code: string; error: string }[];
		tolerated: { name: string; input: string; same: string }[];
	}>('pairing-codes.json');

	it.each(v.valid.map((c) => [c.name, c] as const))('parses %s', (_, c) => {
		expect(parsePairingCode(c.code)).toEqual(c.parsed);
	});

	it.each(v.invalid.map((c) => [c.name, c] as const))('refuses %s', (_, c) => {
		let caught: unknown;
		try {
			parsePairingCode(c.code);
		} catch (error) {
			caught = error;
		}
		expect(caught).toBeInstanceOf(WorkspaceError);
		expect((caught as WorkspaceError).code).toBe(c.error);
	});

	it.each(v.tolerated.map((c) => [c.name, c] as const))('tolerates %s', (_, c) => {
		const same = v.valid.find((valid) => valid.name === c.same);
		expect(same).toBeDefined();
		expect(parsePairingCode(c.input)).toEqual(same?.parsed);
	});
});

describe('conformance: the SDP answer', () => {
	const { cases } = vectors<{
		cases: {
			name: string;
			offer: string[];
			target: ServerTarget;
			ufrag: string;
			now: number;
			answer: string[];
		}[];
	}>('answers.json');
	const sdp = (lines: string[]): string => lines.join('\r\n') + '\r\n';

	it.each(cases.map((c) => [c.name, c] as const))('builds %s', (_, c) => {
		expect(buildServerAnswer(sdp(c.offer), c.target, c.ufrag, c.now)).toBe(sdp(c.answer));
	});
});

describe('conformance: chunks', () => {
	const { cases } = vectors<{ cases: { bytes: number; offset: number; chunks: number[] }[] }>(
		'chunks.json',
	);
	const pieces = (channel: FakeChannel): Uint8Array[] =>
		channel.sent.filter((item): item is Uint8Array => typeof item !== 'string');
	const numbered = (bytes: number): Uint8Array<ArrayBuffer> =>
		Uint8Array.from({ length: bytes }, (_, i) => i % 251);

	it.each(cases.map((c) => [c.bytes, c.offset, c] as const))(
		'sends a body of %i bytes from %i, from memory and from a Blob',
		async (_, __, c) => {
			for (const body of [numbered(c.bytes), new Blob([numbered(c.bytes)])]) {
				const channel = new FakeChannel();
				const framing = new ChannelFraming(
					channel,
					() => undefined,
					() => undefined,
				);

				await framing.sendFrameWithBody({ t: 'put', ifEtag: null }, body, c.offset);

				expect(channel.sentFrames()).toEqual([
					{ t: 'put', ifEtag: null, bytes: c.bytes, offset: c.offset },
				]);
				expect(pieces(channel).map((piece) => piece.byteLength)).toEqual(c.chunks);
				const sent = new Uint8Array(c.bytes - c.offset);
				pieces(channel).reduce((at, piece) => (sent.set(piece, at), at + piece.byteLength), 0);
				expect(sent).toEqual(numbered(c.bytes).subarray(c.offset));
			}
		},
	);

	it.each(cases.map((c) => [c.bytes, c.offset, c] as const))(
		'receives a body of %i bytes from %i',
		(_, __, c) => {
			const channel = new FakeChannel();
			const incoming: Incoming[] = [];
			new ChannelFraming(
				channel,
				(message) => incoming.push(message),
				() => undefined,
			);
			const frame = { t: 'doc', etag: 'e1', bytes: c.bytes, offset: c.offset };

			channel.receiveFrame(frame);
			const body = numbered(c.bytes);
			let at = c.offset;
			for (const size of c.chunks) {
				expect(incoming).toEqual([]);
				channel.receiveBinary(body.subarray(at, at + size));
				at += size;
			}

			expect(incoming).toEqual([{ frame, body: body.subarray(c.offset) }]);
		},
	);
});

interface Call {
	method:
		| 'hello'
		| 'pair'
		| 'head'
		| 'list'
		| 'get'
		| 'begin'
		| 'put'
		| 'delete'
		| 'commit'
		| 'abort'
		| 'resolve'
		| 'forget';
	app?: string;
	device?: string;
	token?: string;
	secret?: string;
	name?: string;
	revision?: string | null;
	cursor?: string;
	path?: string;
	id?: string;
	type?: string;
	etag?: string | null;
	offset?: number;
	tx?: string;
	ifEtag?: string | null;
	body?: string;
	server?: string;
	keep?: Keep;
}

interface Exchange {
	name: string;
	call?: Call;
	client?: Json;
	clientBody?: string;
	push?: string | null;
	pushDiverged?: unknown[];
	pushResolved?: string;
	server: Json[];
	serverBody?: string;
	closes?: boolean;
	result?: unknown;
	error?: string;
}

const keyOf = (call: Call): DocumentKey => ({
	path: call.path ?? '',
	id: call.id ?? '',
	type: call.type ?? '',
});

/** Starts a call on the client; a `get` resolves with its body as text, to compare. */
function start(client: ServerClient, call: Call): Promise<unknown> {
	switch (call.method) {
		case 'hello':
			return client.hello(call.device ?? '', call.token ?? '');
		case 'pair':
			return client.pair(call.secret ?? '', call.name ?? '');
		case 'head':
			return client.head();
		case 'list': {
			const options: { revision?: string | null; cursor?: string } = {};
			if ('revision' in call) options.revision = call.revision;
			if (call.cursor !== undefined) options.cursor = call.cursor;
			return client.list(options);
		}
		case 'get':
			return client
				.get(keyOf(call), { etag: call.etag, offset: call.offset })
				.then((doc) => (isStale(doc) ? doc : { ...doc, body: text(doc.body) }));
		case 'begin':
			return client.begin(call.tx ?? '');
		case 'put':
			return client.put(
				call.tx ?? '',
				keyOf(call),
				call.ifEtag ?? null,
				utf8(call.body ?? ''),
				call.offset,
			);
		case 'delete':
			return client.delete(call.tx ?? '', keyOf(call), call.ifEtag ?? null);
		case 'commit':
			return client.commit(call.tx ?? '');
		case 'abort':
			return client.abort(call.tx ?? '');
		case 'resolve':
			return client.resolve(call.server ?? '', call.keep ?? 'main');
		case 'forget':
			return client.forget().then(() => null);
	}
}

/** Plays the server's frames into the channel, a body after the frame that announces it. */
function playServer(channel: FakeChannel, frames: Json[], body = ''): void {
	for (const frame of frames) {
		// The real server always explains an error; the vectors leave its wording out.
		channel.receiveFrame(
			frame.t === 'error' && frame.message === undefined ? { ...frame, message: 'x' } : frame,
		);
		const offset = typeof frame.offset === 'number' ? frame.offset : 0;
		const length = typeof frame.bytes === 'number' ? frame.bytes - offset : 0;
		if (length > 0) {
			const bytes = utf8(body);
			expect(bytes.byteLength).toBe(length);
			for (let at = 0; at < bytes.byteLength; at += MAX_MESSAGE_BYTES) {
				channel.receiveBinary(bytes.subarray(at, at + MAX_MESSAGE_BYTES));
			}
		}
	}
}

async function expectOutcome(result: Promise<unknown>, entry: Exchange): Promise<void> {
	if (entry.error) {
		await expect(result).rejects.toMatchObject({ name: 'WorkspaceError', code: entry.error });
	} else {
		await expect(result).resolves.toEqual(entry.result);
	}
}

describe('conformance: exchanges', () => {
	const v = vectors<{
		exchanges: Exchange[];
		tolerated: {
			server: { name: string; call: Call; frames: Json[]; result?: unknown; error?: string }[];
		};
	}>('frames.json');

	it.each(v.exchanges.map((e) => [e.name, e] as const))('%s', async (_, ex) => {
		const channel = new FakeChannel();
		const client = new ServerClient(channel, { app: ex.call?.app ?? 'todo' });

		if ('push' in ex || ex.pushDiverged !== undefined || ex.pushResolved !== undefined) {
			const heard: unknown[] = [];
			client.onChanged((revision) => heard.push(revision));
			client.onDiverged((servers) => heard.push(servers));
			client.onResolved((server) => heard.push(server));
			playServer(channel, ex.server);
			expect(heard).toEqual([ex.result]);
			return;
		}

		const call = ex.call;
		if (!call) throw new Error(`${ex.name}: no call`);
		const result = start(client, call);
		result.catch(() => undefined);
		await settle();

		expect(channel.sentFrames()).toEqual([ex.client]);
		const pieces = channel.sent.filter((item): item is Uint8Array => typeof item !== 'string');
		const sent = new Uint8Array(pieces.reduce((n, piece) => n + piece.byteLength, 0));
		pieces.reduce((offset, piece) => (sent.set(piece, offset), offset + piece.byteLength), 0);
		expect(text(sent)).toBe(ex.clientBody ?? '');

		playServer(channel, ex.server, ex.serverBody);
		if (ex.closes) channel.fireClose();

		await expectOutcome(result, ex);
	});

	it.each(v.tolerated.server.map((e) => [e.name, e] as const))('tolerates %s', async (_, tc) => {
		const channel = new FakeChannel();
		const client = new ServerClient(channel, { app: tc.call.app ?? 'todo' });

		const result = start(client, tc.call);
		result.catch(() => undefined);
		await settle();
		playServer(channel, tc.frames);

		await expectOutcome(result, { ...tc, server: tc.frames });
	});
});
