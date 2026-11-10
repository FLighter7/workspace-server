import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import {
	BUFFER_HIGH_BYTES,
	BUFFER_LOW_BYTES,
	ChannelFraming,
	MAX_MESSAGE_BYTES,
} from '../src/ChannelFraming.js';
import { PAIRING_CODE_VERSION, parsePairingCode, type ServerTarget } from '../src/PairingCode.js';
import { buildServerAnswer } from '../src/serverAnswer.js';
import { PROTOCOL_VERSION, ServerClient } from '../src/ServerClient.js';
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
		}>('constants.json');

		expect(PROTOCOL_VERSION).toBe(c.proto);
		expect(`wsrv${PAIRING_CODE_VERSION}.`).toBe(c.pairingCodePrefix);
		expect(MAX_MESSAGE_BYTES).toBe(c.chunkBytes);
		expect(BUFFER_HIGH_BYTES).toBe(c.bufferHighBytes);
		expect(BUFFER_LOW_BYTES).toBe(c.bufferLowBytes);
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
	const { cases } = vectors<{ cases: { bytes: number; chunks: number[] }[] }>('chunks.json');

	it.each(cases.map((c) => [c.bytes, c] as const))('sends a body of %i bytes', async (_, c) => {
		const channel = new FakeChannel();
		const framing = new ChannelFraming(
			channel,
			() => undefined,
			() => undefined,
		);

		await framing.sendFrameWithBody({ t: 'put', ifEtag: null }, new Uint8Array(c.bytes));

		expect(channel.sentFrames()).toEqual([{ t: 'put', ifEtag: null, bytes: c.bytes }]);
		const pieces = channel.sent.filter((item): item is Uint8Array => typeof item !== 'string');
		expect(pieces.map((piece) => piece.byteLength)).toEqual(c.chunks);
	});
});

interface Call {
	method: 'hello' | 'pair' | 'head' | 'get' | 'put' | 'forget';
	app?: string;
	device?: string;
	token?: string;
	secret?: string;
	name?: string;
	ifEtag?: string | null;
	body?: string;
}

interface Exchange {
	name: string;
	call?: Call;
	client?: Json;
	clientBody?: string;
	push?: string;
	server: Json[];
	serverBody?: string;
	closes?: boolean;
	result?: unknown;
	error?: string;
}

/** Starts a call on the client; a `get` resolves with its body as text, to compare. */
function start(client: ServerClient, call: Call): Promise<unknown> {
	switch (call.method) {
		case 'hello':
			return client.hello(call.device ?? '', call.token ?? '');
		case 'pair':
			return client.pair(call.secret ?? '', call.name ?? '');
		case 'head':
			return client.head();
		case 'get':
			return client.get().then((doc) => ({ etag: doc.etag, body: text(doc.body) }));
		case 'put':
			return client.put(call.ifEtag ?? null, utf8(call.body ?? ''));
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
		if (typeof frame.bytes === 'number' && frame.bytes > 0) {
			const bytes = utf8(body);
			for (let offset = 0; offset < bytes.byteLength; offset += MAX_MESSAGE_BYTES) {
				channel.receiveBinary(bytes.subarray(offset, offset + MAX_MESSAGE_BYTES));
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

		if (ex.push !== undefined) {
			const heard: string[] = [];
			client.onChanged((etag) => heard.push(etag));
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
