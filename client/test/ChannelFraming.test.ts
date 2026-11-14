import { describe, expect, it, vi } from 'vitest';
import { ChannelFraming, MAX_MESSAGE_BYTES, type Incoming } from '../src/ChannelFraming.js';
import { WorkspaceError } from '../src/WorkspaceError.js';
import { FakeChannel } from '../src/testing/FakeChannel.js';

function setup() {
	const channel = new FakeChannel();
	const incoming: Incoming[] = [];
	const failures: WorkspaceError[] = [];
	const framing = new ChannelFraming(
		channel,
		(message) => incoming.push(message),
		(error) => failures.push(error),
	);

	return { channel, incoming, failures, framing };
}

/** Lets pending promise callbacks run. */
async function flush(): Promise<void> {
	for (let i = 0; i < 5; i++) await Promise.resolve();
}

describe('ChannelFraming', () => {
	it('delivers a plain frame', () => {
		const { channel, incoming, failures } = setup();

		channel.receiveFrame({ t: 'head', etag: 'abc' });

		expect(incoming).toEqual([{ frame: { t: 'head', etag: 'abc' }, body: null }]);
		expect(failures).toEqual([]);
	});

	it('assembles a body from chunks', () => {
		const { channel, incoming } = setup();

		channel.receiveFrame({ t: 'doc', etag: 'e', bytes: 5 });
		channel.receiveBinary([1, 2]);
		expect(incoming).toEqual([]);
		channel.receiveBinary([3, 4, 5]);

		expect(incoming).toEqual([
			{ frame: { t: 'doc', etag: 'e', bytes: 5 }, body: Uint8Array.from([1, 2, 3, 4, 5]) },
		]);
	});

	it('delivers an empty body at once', () => {
		const { channel, incoming } = setup();

		channel.receiveFrame({ t: 'doc', etag: null, bytes: 0 });

		expect(incoming).toEqual([
			{ frame: { t: 'doc', etag: null, bytes: 0 }, body: new Uint8Array(0) },
		]);
	});

	it('fails on binary out of turn', () => {
		const { channel, failures } = setup();

		channel.receiveBinary([1]);

		expect(failures.map((error) => error.code)).toEqual(['protocol']);
	});

	it('fails on overrun', () => {
		const { channel, incoming, failures } = setup();

		channel.receiveFrame({ t: 'doc', etag: 'e', bytes: 2 });
		channel.receiveBinary([1, 2, 3]);

		expect(incoming).toEqual([]);
		expect(failures.map((error) => error.code)).toEqual(['protocol']);
	});

	it('fails on a frame without t', () => {
		const { channel, failures } = setup();

		channel.receiveFrame({ etag: 'e' });

		expect(failures.map((error) => error.code)).toEqual(['protocol']);
	});

	it('fails on text that is not JSON', () => {
		const { channel, failures } = setup();

		channel.receiveText('{');

		expect(failures).toHaveLength(1);
		expect(failures[0]).toBeInstanceOf(WorkspaceError);
		expect(failures[0].code).toBe('protocol');
	});

	it('reports a failure once and ignores what follows', () => {
		const { channel, incoming, failures } = setup();

		channel.receiveBinary([1]);
		channel.receiveFrame({ t: 'head', etag: null });

		expect(failures).toHaveLength(1);
		expect(incoming).toEqual([]);
	});

	it('sends a body in 16 KiB chunks', async () => {
		const { channel, framing } = setup();
		const body = Uint8Array.from({ length: 40000 }, (_, i) => i % 251);

		await framing.sendFrameWithBody({ t: 'put', ifEtag: null }, body);

		expect(JSON.parse(channel.sent[0] as string)).toEqual({
			t: 'put',
			ifEtag: null,
			bytes: 40000,
		});
		const chunks = channel.sent.slice(1) as Uint8Array[];
		expect(chunks.map((chunk) => chunk.byteLength)).toEqual([
			MAX_MESSAGE_BYTES,
			MAX_MESSAGE_BYTES,
			7232,
		]);
		const joined = new Uint8Array(40000);
		let offset = 0;
		for (const chunk of chunks) {
			joined.set(chunk, offset);
			offset += chunk.byteLength;
		}
		expect(joined).toEqual(body);
		expect(channel.bufferedAmountLowThreshold).toBe(256 * 1024);
	});

	it('waits while the buffer is full', async () => {
		const { channel, framing } = setup();
		const realSend = channel.send.bind(channel);
		vi.spyOn(channel, 'send').mockImplementation((data) => {
			realSend(data);
			// The first piece fills the buffer past the high mark.
			if (typeof data !== 'string') channel.bufferedAmount = 2 * 1024 * 1024;
		});

		const sending = framing.sendFrameWithBody({ t: 'put', ifEtag: null }, new Uint8Array(20000));
		await flush();
		expect(channel.sent).toHaveLength(2);

		channel.bufferedAmount = 0;
		channel.fireLow();
		await sending;

		expect(channel.sent).toHaveLength(3);
	});

	it('rejects a body whose channel closes mid-send', async () => {
		const { channel, framing } = setup();
		channel.bufferedAmount = 2 * 1024 * 1024;

		const sending = framing.sendFrameWithBody({ t: 'put', ifEtag: null }, new Uint8Array(10));
		channel.fireClose();

		await expect(sending).rejects.toMatchObject({ code: 'closed' });
	});

	it('dispose removes listeners', () => {
		const { channel, incoming, framing } = setup();
		expect(channel.listenerCount('message')).toBe(1);

		framing.dispose();
		channel.receiveFrame({ t: 'head', etag: null });

		expect(channel.listenerCount('message')).toBe(0);
		expect(incoming).toEqual([]);
	});

	it('expects only the bytes past an offset', () => {
		const { channel, incoming } = setup();

		channel.receiveFrame({ t: 'doc', etag: 'e', bytes: 5, offset: 3 });
		channel.receiveBinary([4, 5]);

		expect(incoming).toEqual([
			{ frame: { t: 'doc', etag: 'e', bytes: 5, offset: 3 }, body: Uint8Array.from([4, 5]) },
		]);
	});

	it('delivers a body whose offset is its size at once, empty', () => {
		const { channel, incoming } = setup();

		channel.receiveFrame({ t: 'doc', etag: 'e', bytes: 5, offset: 5 });

		expect(incoming).toEqual([
			{ frame: { t: 'doc', etag: 'e', bytes: 5, offset: 5 }, body: new Uint8Array(0) },
		]);
	});

	it('fails on an offset past the size', () => {
		const { channel, failures } = setup();

		channel.receiveFrame({ t: 'doc', etag: 'e', bytes: 5, offset: 6 });

		expect(failures.map((error) => error.code)).toEqual(['protocol']);
	});

	it('only a doc announces a body: bytes in any other frame is just a field', () => {
		const { channel, incoming, failures } = setup();

		channel.receiveFrame({ t: 'began', tx: 'tx', staged: [], bytes: 5, offset: 1 });
		channel.receiveFrame({ t: 'head', revision: null });

		expect(incoming).toEqual([
			{ frame: { t: 'began', tx: 'tx', staged: [], bytes: 5, offset: 1 }, body: null },
			{ frame: { t: 'head', revision: null }, body: null },
		]);
		channel.receiveBinary([1]);
		expect(failures.map((error) => error.code)).toEqual(['protocol']);
	});

	it('fails on a doc without a size', () => {
		const { channel, failures } = setup();

		channel.receiveFrame({ t: 'doc', etag: 'e' });

		expect(failures.map((error) => error.code)).toEqual(['protocol']);
	});

	it('streams a body to the receiver onBody gives, and delivers the frame after it', () => {
		const channel = new FakeChannel();
		const incoming: Incoming[] = [];
		const pieces: number[][] = [];
		new ChannelFraming(
			channel,
			(message) => incoming.push(message),
			() => undefined,
			(frame) => (frame.t === 'doc' ? (piece) => pieces.push([...piece]) : null),
		);

		channel.receiveFrame({ t: 'doc', etag: 'e', bytes: 3 });
		channel.receiveBinary([1, 2]);
		expect(pieces).toEqual([[1, 2]]);
		expect(incoming).toEqual([]);
		channel.receiveBinary([3]);

		expect(pieces).toEqual([[1, 2], [3]]);
		expect(incoming).toEqual([
			{ frame: { t: 'doc', etag: 'e', bytes: 3 }, body: new Uint8Array(0), streamed: true },
		]);
	});

	it('sends a Blob from an offset, read a slice at a time', async () => {
		const { channel, framing } = setup();
		const bytes = Uint8Array.from({ length: 40000 }, (_, i) => i % 251);

		await framing.sendFrameWithBody({ t: 'put' }, new Blob([bytes]), 1000);

		expect(channel.sentFrames()).toEqual([{ t: 'put', bytes: 40000, offset: 1000 }]);
		const chunks = channel.sent.slice(1) as Uint8Array[];
		expect(chunks.map((chunk) => chunk.byteLength)).toEqual([16384, 16384, 6232]);
		expect(chunks[0][0]).toBe(1000 % 251);
	});

	it('tells the position in the body after every piece it hands on', async () => {
		const { channel, framing } = setup();
		const positions: number[] = [];
		const size = 2 * MAX_MESSAGE_BYTES + 10;

		await framing.sendFrameWithBody({ t: 'put' }, new Uint8Array(size), 5, (at) =>
			positions.push(at),
		);

		expect(positions).toEqual([5 + MAX_MESSAGE_BYTES, 5 + 2 * MAX_MESSAGE_BYTES, size]);
		expect(channel.sent).toHaveLength(4);
	});

	it('refuses an offset past the body before sending anything', async () => {
		const { channel, framing } = setup();

		await expect(
			framing.sendFrameWithBody({ t: 'put' }, new Uint8Array(2), 3),
		).rejects.toBeInstanceOf(RangeError);
		expect(channel.sent).toEqual([]);
	});
});
