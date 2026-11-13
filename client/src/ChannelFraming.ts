import { WorkspaceError } from './WorkspaceError.js';

/**
 * The largest single binary message this sends. 16 KiB is the size every
 * browser has carried reliably, and what the server's framing allows.
 */
export const MAX_MESSAGE_BYTES = 16 * 1024;

/** Stop queueing pieces once this much waits to go out, resume at the low mark. */
export const BUFFER_HIGH_BYTES = 1024 * 1024;
export const BUFFER_LOW_BYTES = 256 * 1024;

/** How much of a `Blob` body is read at a time, to be sent in pieces. */
const BLOB_SLICE_BYTES = 1024 * 1024;

/** The part of RTCDataChannel this needs; an RTCDataChannel satisfies it. */
export interface FrameChannel {
	readonly readyState: RTCDataChannelState;
	readonly bufferedAmount: number;
	bufferedAmountLowThreshold: number;
	send(data: string | ArrayBufferView<ArrayBuffer>): void;
	close(): void;
	addEventListener(
		type: 'message',
		listener: (event: MessageEvent<string | ArrayBuffer>) => void,
	): void;
	addEventListener(type: 'bufferedamountlow' | 'close', listener: () => void): void;
	removeEventListener(
		type: 'message',
		listener: (event: MessageEvent<string | ArrayBuffer>) => void,
	): void;
	removeEventListener(type: 'bufferedamountlow' | 'close', listener: () => void): void;
}

/** A text message: a JSON object with a `t` that says what it is. */
export interface Frame {
	t: string;
	[key: string]: unknown;
}

/**
 * A frame as delivered: with the body it announced, or `null` when it
 * announced none. A body handed piece by piece to the receiver `onBody` gave
 * is delivered empty, with `streamed` set.
 */
export interface Incoming {
	frame: Frame;
	body: Uint8Array | null;
	streamed?: true;
}

/** A body to send: the bytes in memory, or a `Blob` (a `File` included), read as it goes. */
export type Body = Uint8Array | Blob;

/**
 * Asked when a frame announces a body: a function that takes its pieces as
 * they arrive, or `null` to have the body assembled and delivered whole.
 */
export type BodyReceiver = (frame: Frame) => ((piece: Uint8Array) => void) | null;

function protocolError(message: string): WorkspaceError {
	return new WorkspaceError(message, 'protocol');
}

function isFrame(value: unknown): value is Frame {
	return (
		typeof value === 'object' &&
		value !== null &&
		!Array.isArray(value) &&
		typeof (value as { t?: unknown }).t === 'string'
	);
}

function isCount(value: unknown): value is number {
	return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

/** Resolves once the channel's send buffer drains to its low mark, or the channel closes. */
function drained(channel: FrameChannel): Promise<void> {
	return new Promise((resolve) => {
		const done = (): void => {
			channel.removeEventListener('bufferedamountlow', done);
			channel.removeEventListener('close', done);
			resolve();
		};

		channel.addEventListener('bufferedamountlow', done);
		channel.addEventListener('close', done);
	});
}

/** The size of a body to send. */
export function bodySize(body: Body): number {
	return body instanceof Uint8Array ? body.byteLength : body.size;
}

/**
 * Text frames and the bodies some of them announce, over one data channel,
 * as the server's `docs/protocol.md` ("Framing") lays them out: a frame
 * carrying `bytes` (and `offset`, 0 when absent) is followed by the body's
 * bytes past the offset, in binary messages of at most 16 KiB. Not a
 * singleton: one per channel.
 */
export class ChannelFraming {
	/** The frame whose body is arriving, where its pieces go, and how much is still to come. */
	private pending: {
		frame: Frame;
		left: number;
		chunks: Uint8Array[] | null;
		total: number;
		receive: ((piece: Uint8Array) => void) | null;
	} | null = null;
	private failed = false;

	private readonly onMessage = (event: MessageEvent<string | ArrayBuffer>): void => {
		if (this.failed) return;
		try {
			if (typeof event.data === 'string') this.readText(event.data);
			else this.readBinary(new Uint8Array(event.data));
		} catch (error) {
			this.failed = true;
			this.onFailure(
				error instanceof WorkspaceError ? error : protocolError('The server sent a broken frame'),
			);
		}
	};

	constructor(
		private readonly channel: FrameChannel,
		private readonly onIncoming: (message: Incoming) => void,
		private readonly onFailure: (error: WorkspaceError) => void,
		private readonly onBody: BodyReceiver = () => null,
	) {
		channel.addEventListener('message', this.onMessage);
	}

	sendFrame(frame: Frame): void {
		this.channel.send(JSON.stringify(frame));
	}

	/**
	 * Sends a frame announcing `body` (its `bytes`, and `offset` when one is
	 * given), then the body's bytes past `offset` in pieces of at most 16 KiB
	 * counted from the offset, pausing while the channel's buffer holds more
	 * than 1 MiB. A `Blob` is read a slice at a time, never whole. Resolves
	 * once every piece is handed to the channel; rejects if the channel
	 * closes first, or if the `Blob` cannot be read.
	 */
	async sendFrameWithBody(frame: Frame, body: Body, offset?: number): Promise<void> {
		const size = bodySize(body);
		const from = offset ?? 0;
		if (!isCount(from) || from > size) {
			throw new RangeError(`An offset of ${String(offset)} into a body of ${size} bytes`);
		}
		this.sendFrame(
			offset === undefined ? { ...frame, bytes: size } : { ...frame, bytes: size, offset },
		);

		this.channel.bufferedAmountLowThreshold = BUFFER_LOW_BYTES;
		for (let start = from; start < size;) {
			const slice =
				body instanceof Uint8Array
					? body.subarray(start, size)
					: new Uint8Array(
							await body.slice(start, Math.min(size, start + BLOB_SLICE_BYTES)).arrayBuffer(),
						);
			for (let at = 0; at < slice.byteLength; at += MAX_MESSAGE_BYTES) {
				if (this.channel.bufferedAmount > BUFFER_HIGH_BYTES) await drained(this.channel);
				if (this.channel.readyState !== 'open') {
					throw new WorkspaceError('The channel closed while sending', 'closed');
				}
				this.channel.send(slice.subarray(at, at + MAX_MESSAGE_BYTES) as Uint8Array<ArrayBuffer>);
			}
			start += slice.byteLength;
		}
	}

	dispose(): void {
		this.channel.removeEventListener('message', this.onMessage);
	}

	private readText(text: string): void {
		if (this.pending) throw protocolError('A text frame arrived inside a body');

		const frame: unknown = JSON.parse(text);
		if (!isFrame(frame)) throw protocolError('A frame without t');

		const { bytes, offset } = frame;
		if (bytes === undefined) {
			this.onIncoming({ frame, body: null });
			return;
		}

		if (!isCount(bytes) || (offset !== undefined && (!isCount(offset) || offset > bytes))) {
			throw protocolError('A frame with an invalid body size or offset');
		}

		const length = bytes - (offset ?? 0);
		if (length === 0) {
			this.onIncoming({ frame, body: new Uint8Array(0) });
			return;
		}

		const receive = this.onBody(frame);
		this.pending = { frame, left: length, total: length, chunks: receive ? null : [], receive };
	}

	private readBinary(piece: Uint8Array): void {
		const pending = this.pending;
		if (!pending) throw protocolError('A binary message arrived when no body was expected');
		if (piece.byteLength > pending.left) throw protocolError('A body overran its announced size');

		pending.left -= piece.byteLength;
		if (pending.receive) pending.receive(piece);
		else pending.chunks?.push(piece);
		if (pending.left > 0) return;

		this.pending = null;
		if (!pending.chunks) {
			this.onIncoming({ frame: pending.frame, body: new Uint8Array(0), streamed: true });
			return;
		}

		const body = new Uint8Array(pending.total);
		let at = 0;
		for (const chunk of pending.chunks) {
			body.set(chunk, at);
			at += chunk.byteLength;
		}
		this.onIncoming({ frame: pending.frame, body });
	}
}
