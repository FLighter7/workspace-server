import { reactive } from 'vue';
import { serverApi } from '@/api/client';
import type { Pairing } from '@/api/types';

/** A pairing code on screen, counted down every second. */
export interface PairingCodeState {
	/** The QR code as a data: URL; "" when no code is on screen. */
	qr: string;
	text: string;
	expiresAt: number;
	/** Whole seconds left. */
	left: number;
	/** The code on screen ran out; shown until a new one arrives. */
	expired: boolean;
	/**
	 * The machine the code is for: the device that pairs with it joins that
	 * physical device of the user's; "" for a new one.
	 */
	machine: string;
}

export interface PairingCode {
	readonly state: PairingCodeState;
	/**
	 * Asks for a new code for machine (the one the code on screen is for when
	 * left out), which revokes the previous one, and shows it. A call while one
	 * is in flight for the same machine does nothing; one for another machine
	 * is made once it ends, so the code shown is always for the last one asked.
	 * Rejects with the API's error.
	 */
	issue: (machine?: string) => Promise<void>;
	/** The code on screen is still valid: opening its dialog again keeps it. */
	valid: () => boolean;
	/** Forgets the code on screen, so that opening its dialog again issues a new one. */
	drop: () => void;
}

/**
 * A pairing code that outlives its dialog, so that opening the dialog again
 * does not revoke a code still valid, and that another section can drop when
 * a change makes it wrong.
 */
export function createPairingCode(request: (machine: string) => Promise<Pairing>): PairingCode {
	const state = reactive<PairingCodeState>({
		qr: '',
		text: '',
		expiresAt: 0,
		left: 0,
		expired: false,
		machine: '',
	});
	let countdown: ReturnType<typeof setInterval> | undefined;
	/** The machine a request in flight is for; null when none is. */
	let issuing: string | null = null;
	/** The machine asked for while another was in flight. */
	let next: string | null = null;

	function hide(): void {
		clearInterval(countdown);
		state.qr = '';
		state.text = '';
		state.left = 0;
	}

	function tick(): void {
		state.left = Math.max(0, Math.ceil((state.expiresAt - Date.now()) / 1000));
		if (state.left === 0) {
			hide();
			state.expired = true;
		}
	}

	async function issue(machine = state.machine): Promise<void> {
		if (issuing !== null) {
			next = machine === issuing ? null : machine;
			return;
		}
		issuing = machine;
		try {
			const pairing = await request(machine);
			state.qr = pairing.qr;
			state.text = pairing.code;
			state.expiresAt = Date.parse(pairing.expiresAt);
			state.expired = false;
			state.machine = machine;
			clearInterval(countdown);
			countdown = setInterval(tick, 1000);
			tick();
		} catch (error) {
			// The error is said; whatever was asked meanwhile is asked again by hand.
			next = null;
			throw error;
		} finally {
			issuing = null;
		}
		if (next !== null) {
			const then = next;
			next = null;
			await issue(then);
		}
	}

	return {
		state,
		issue,
		valid: () => state.expiresAt > Date.now(),
		drop() {
			state.expiresAt = 0;
			state.expired = false;
			state.machine = '';
			next = null;
			hide();
		},
	};
}

/**
 * This server's own code, from Devices → Pair a device. It carries the
 * server's name and UDP port, and a reset revokes it: the Server section
 * drops it then.
 */
export const ownCode = createPairingCode((machine) => serverApi.issuePairing(machine));

/**
 * The main's code, which a subordinate's page asks the main for, for a
 * browser on this computer. Issuing it revokes the code the main's own page
 * shows, as a new code always does.
 */
export const invitedCode = createPairingCode((machine) => serverApi.invite(machine));

/** "m:ss" for the seconds left. */
export function formatLeft(seconds: number): string {
	return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}
