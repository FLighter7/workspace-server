import type { FrameChannel } from '../ChannelFraming.js';

type Listener = (...args: never[]) => void;

/**
 * A data channel stand-in for the framing and client tests: it records what
 * is sent, and lets a test play the server's side — deliver text or binary,
 * drain the buffer, close.
 */
export class FakeChannel implements FrameChannel {
	readyState: RTCDataChannelState = 'open';
	bufferedAmount = 0;
	bufferedAmountLowThreshold = 0;
	readonly sent: (string | Uint8Array)[] = [];
	private readonly listeners = new Map<string, Set<Listener>>();

	send(data: string | ArrayBufferView<ArrayBuffer>): void {
		if (this.readyState !== 'open') throw new Error('The channel is not open');
		this.sent.push(
			typeof data === 'string'
				? data
				: new Uint8Array(data.buffer, data.byteOffset, data.byteLength).slice(),
		);
	}

	close(): void {
		if (this.readyState === 'closed') return;
		this.fireClose();
	}

	addEventListener(type: string, listener: Listener): void {
		let set = this.listeners.get(type);
		if (!set) {
			set = new Set();
			this.listeners.set(type, set);
		}
		set.add(listener);
	}

	removeEventListener(type: string, listener: Listener): void {
		this.listeners.get(type)?.delete(listener);
	}

	listenerCount(type: string): number {
		return this.listeners.get(type)?.size ?? 0;
	}

	/** The text frames sent so far, parsed. */
	sentFrames(): Record<string, unknown>[] {
		return this.sent
			.filter((item): item is string => typeof item === 'string')
			.map((item) => JSON.parse(item) as Record<string, unknown>);
	}

	receiveText(text: string): void {
		this.emit('message', { data: text });
	}

	receiveFrame(frame: Record<string, unknown>): void {
		this.receiveText(JSON.stringify(frame));
	}

	receiveBinary(bytes: Uint8Array | number[]): void {
		const copy = Uint8Array.from(bytes);
		this.emit('message', { data: copy.buffer });
	}

	fireLow(): void {
		this.emit('bufferedamountlow');
	}

	fireClose(): void {
		this.readyState = 'closed';
		this.emit('close');
	}

	private emit(type: string, event?: unknown): void {
		for (const listener of [...(this.listeners.get(type) ?? [])]) {
			(listener as (event: unknown) => void)(event);
		}
	}
}
