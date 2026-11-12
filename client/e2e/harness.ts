/** What the e2e page (`page/harness.js`) puts on `window`, for the specs. */

/** A device: the built package's engine over a set of notes, merged as a union. */
export interface Device {
	state: { notes: string[]; status: string; via: string | null; error: string | null };
	pair(code: string): Promise<string>;
	add(note: string): void;
	/** The app hidden and shown again: the engine reconnects. */
	reconnect(): void;
}

declare global {
	interface Window {
		harness: {
			createDevice(notes: string[]): Device;
			pairAs(app: string, code: string): Promise<string>;
		};
		device: Device;
	}
}
