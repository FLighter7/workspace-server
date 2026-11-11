import { reactive } from 'vue';
import { serverApi } from '@/api/client';

/**
 * The pairing code on screen. It outlives the Pair section, so that coming
 * back to it does not revoke a code still valid, and the Server section can
 * drop it when a change makes it wrong.
 */
export const pairingCode = reactive({
	/** The QR code as a data: URL; "" when no code is on screen. */
	qr: '',
	text: '',
	expiresAt: 0,
	/** Whole seconds left, counted down every second. */
	left: 0,
	/** The code on screen ran out; shown until a new one arrives. */
	expired: false,
});

let countdown: ReturnType<typeof setInterval> | undefined;
let issuing = false;

function hide(): void {
	clearInterval(countdown);
	pairingCode.qr = '';
	pairingCode.text = '';
	pairingCode.left = 0;
}

function tick(): void {
	pairingCode.left = Math.max(0, Math.ceil((pairingCode.expiresAt - Date.now()) / 1000));
	if (pairingCode.left === 0) {
		hide();
		pairingCode.expired = true;
	}
}

/** A code on screen is still valid: entering the section keeps it. */
export const codeValid = (): boolean => pairingCode.expiresAt > Date.now();

/**
 * Asks for a new secret, which revokes the previous one, and shows its code.
 * A call while one is in flight does nothing. Rejects with the API's error.
 */
export async function issueCode(): Promise<void> {
	if (issuing) return;
	issuing = true;
	try {
		const pairing = await serverApi.issuePairing();
		pairingCode.qr = pairing.qr;
		pairingCode.text = pairing.code;
		pairingCode.expiresAt = Date.parse(pairing.expiresAt);
		pairingCode.expired = false;
		clearInterval(countdown);
		countdown = setInterval(tick, 1000);
		tick();
	} finally {
		issuing = false;
	}
}

/**
 * Forgets the code on screen, so that entering the section again issues a new
 * one: a code carries the server's name and UDP port, and a reset revokes it.
 */
export function dropCode(): void {
	pairingCode.expiresAt = 0;
	pairingCode.expired = false;
	hide();
}

/** "m:ss" for the seconds left. */
export function formatLeft(seconds: number): string {
	return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}
