import type { ServerTarget } from './PairingCode.js';

const UFRAG_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
const UFRAG_LENGTH = 8;
/** The priority of the first host candidate; each later one is one lower, so the order holds. */
const FIRST_PRIORITY = 2130706431;

/**
 * A fresh ICE username fragment for the server's side of one connection.
 * The server keys connections by it, so it must be new every time.
 */
export function randomIceUfrag(): string {
	const bytes = crypto.getRandomValues(new Uint8Array(UFRAG_LENGTH));

	// `% 62` slightly favours the first few characters. Harmless: the ufrag
	// only has to differ between connections, it is no secret.
	return Array.from(bytes, (byte) => UFRAG_ALPHABET[byte % UFRAG_ALPHABET.length]).join('');
}

/**
 * The server's SDP answer to this device's offer, built here instead of
 * asked for: the server is ICE-lite with a fixed password and certificate,
 * so everything in the answer is already known from the pairing, and no
 * signaling exchange is needed. The layout follows the server's
 * `docs/protocol.md`, "The client's side of the connection", line by line;
 * `testdata/conformance/answers.json` pins it.
 *
 * `now` (milliseconds) only fills the session version in the origin line.
 */
export function buildServerAnswer(
	offerSdp: string,
	target: ServerTarget,
	ufrag: string,
	now: number = Date.now(),
): string {
	const mid = /^a=mid:(\S+)/m.exec(offerSdp)?.[1] ?? '0';
	const candidates = target.hosts.map(
		(host, i) => `a=candidate:${i + 1} 1 udp ${FIRST_PRIORITY - i} ${host} ${target.port} typ host`,
	);

	const lines = [
		'v=0',
		`o=- ${now} 2 IN IP4 127.0.0.1`,
		's=-',
		't=0 0',
		`a=group:BUNDLE ${mid}`,
		'a=ice-lite',
		'a=msid-semantic: WMS',
		`m=application ${target.port} UDP/DTLS/SCTP webrtc-datachannel`,
		'c=IN IP4 0.0.0.0',
		...candidates,
		'a=end-of-candidates',
		`a=ice-ufrag:${ufrag}`,
		`a=ice-pwd:${target.password}`,
		`a=fingerprint:sha-256 ${target.fingerprint}`,
		'a=setup:passive',
		`a=mid:${mid}`,
		'a=sctp-port:5000',
		'a=max-message-size:262144',
	];

	return lines.join('\r\n') + '\r\n';
}
