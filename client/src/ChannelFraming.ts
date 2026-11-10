import { WorkspaceError } from './WorkspaceError.js';

/**
 * The largest single binary message this sends. 16 KiB is the size every
 * browser has carried reliably, and what the server's framing allows.
 */
export const MAX_MESSAGE_BYTES = 16 * 1024;

/** Stop queueing pieces once this much waits to go out, resume at the low mark. */
export const BUFFER_HIGH_BYTES = 1024 * 1024;
export const BUFFER_LOW_BYTES = 256 * 1024;

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

/** A frame as delivered: with the body it announced, or `null` when it announced none. */
export interface Incoming {
	frame: Frame;
	body: Uint8Array | null;
}

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

/**
 * Text frames and the bodies some of them announce, over one data channel,
 * as the server's `docs/protocol.md` ("Framing") lays them out: a frame
 * carrying `bytes` is followed by exactly that many bytes in binary
 * messages of at most 16 KiB. Not a singleton: one per channel.
 */
export class ChannelFraming {
	/** The frame whose body is arriving, the pieces so far, and how much of it is in. */
	private pending: {
		frame: Frame;
		expected: number;
		chunks: Uint8Array[];
		received: number;
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
	) {
		channel.addEventListener('message', this.onMessage);
	}

	sendFrame(frame: Frame): void {
		this.channel.send(JSON.stringify(frame));
	}

	/**
	 * Sends a frame announcing `body`, then the body in pieces, pausing while
	 * the channel's buffer holds more than 1 MiB. Resolves once every piece
	 * is handed to the channel; rejects if the channel closes first.
	 */
	async sendFrameWithBody(frame: Frame, body: Uint8Array): Promise<void> {
		this.sendFrame({ ...frame, bytes: body.byteLength });

		this.channel.bufferedAmountLowThreshold = BUFFER_LOW_BYTES;
		for (let offset = 0; offset < body.byteLength; offset += MAX_MESSAGE_BYTES) {
			if (this.channel.bufferedAmount > BUFFER_HIGH_BYTES) await drained(this.channel);
			if (this.channel.readyState !== 'open') {
				throw new WorkspaceError('The channel closed while sending', 'closed');
			}
			this.channel.send(
				body.subarray(offset, offset + MAX_MESSAGE_BYTES) as Uint8Array<ArrayBuffer>,
			);
		}
	}

	dispose(): void {
		this.channel.removeEventListener('message', this.onMessage);
	}

	private readText(text: string): void {
		if (this.pending) throw protocolError('A text frame arrived inside a body');

		const frame: unknown = JSON.parse(text);
		if (!isFrame(frame)) throw protocolError('A frame without t');

		const { bytes } = frame;
		if (bytes === undefined) {
			this.onIncoming({ frame, body: null });
			return;
		}

		if (typeof bytes !== 'number' || !Number.isSafeInteger(bytes) || bytes < 0) {
			throw protocolError('A frame with an invalid body size');
		}

		if (bytes === 0) {
			this.onIncoming({ frame, body: new Uint8Array(0) });
			return;
		}

		this.pending = { frame, expected: bytes, chunks: [], received: 0 };
	}

	private readBinary(chunk: Uint8Array): void {
		const pending = this.pending;
		if (!pending) throw protocolError('A binary message arrived when no body was expected');
		if (pending.received + chunk.byteLength > pending.expected) {
			throw protocolError('A body overran its announced size');
		}

		pending.chunks.push(chunk);
		pending.received += chunk.byteLength;
		if (pending.received < pending.expected) return;

		const body = new Uint8Array(pending.expected);
		let offset = 0;
		for (const piece of pending.chunks) {
			body.set(piece, offset);
			offset += piece.byteLength;
		}

		this.pending = null;
		this.onIncoming({ frame: pending.frame, body });
	}
}
