import { WorkspaceError } from './WorkspaceError.js';

/** What a device needs to reach the server: everything the answer it builds is made from. */
export interface ServerTarget {
	/** The server's fixed ICE password. */
	password: string;
	/** The SHA-256 fingerprint of the server's certificate, uppercase colon-separated hex. */
	fingerprint: string;
	/** The server's UDP port. */
	port: number;
	/** The server's `.local` name first, then its LAN IPv4 addresses. */
	hosts: string[];
}

/** A parsed `wsrv1.` pairing code, as the server's page shows it. */
export interface PairingCode extends ServerTarget {
	serverId: string;
	name: string;
	/** The one-time secret that proves the user stood at the server's screen. */
	secret: string;
}

const CODE_PATTERN = /^wsrv(\d+)\.([A-Za-z0-9_-]+)$/;
/** The pairing code version this client reads: codes start with `wsrv1.`. */
export const PAIRING_CODE_VERSION = 1;
const FINGERPRINT_BYTES = 32;
/** The server's ICE password is 24 characters; ICE itself asks for at least 22. */
const MIN_PASSWORD_LENGTH = 22;

/** The JSON inside the code, before any of it is trusted. */
interface RawCode {
	v: unknown;
	id: unknown;
	n: unknown;
	p: unknown;
	f: unknown;
	o: unknown;
	h: unknown;
	s: unknown;
}

function damaged(): WorkspaceError {
	return new WorkspaceError('The pairing code is damaged', 'damaged');
}

/** Decodes unpadded base64url; throws on anything that is not. */
function fromBase64Url(value: string): Uint8Array {
	if (value.length % 4 === 1) throw damaged();
	const binary = atob(value.replace(/-/g, '+').replace(/_/g, '/'));

	return Uint8Array.from(binary, (ch) => ch.charCodeAt(0));
}

function isNonEmptyString(value: unknown): value is string {
	return typeof value === 'string' && value.length > 0;
}

function toColonHex(bytes: Uint8Array): string {
	return Array.from(bytes, (byte) => byte.toString(16).toUpperCase().padStart(2, '0')).join(':');
}

function readFingerprint(value: unknown): string {
	if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value)) throw damaged();
	const bytes = fromBase64Url(value);
	if (bytes.length !== FINGERPRINT_BYTES) throw damaged();

	return toColonHex(bytes);
}

function readJson(payload: string): Partial<RawCode> {
	let parsed: unknown;
	try {
		const text = new TextDecoder('utf-8', { fatal: true }).decode(fromBase64Url(payload));
		parsed = JSON.parse(text);
	} catch {
		throw damaged();
	}

	if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) throw damaged();

	return parsed as Partial<RawCode>;
}

/**
 * Reads a pairing code scanned or pasted from the server's page. Stray
 * whitespace around it is ignored; anything else wrong throws a
 * `WorkspaceError` whose code says what.
 */
export function parsePairingCode(text: string): PairingCode {
	const match = CODE_PATTERN.exec(text.trim());
	if (!match) {
		throw new WorkspaceError('This is not a workspace-server pairing code', 'notPairingCode');
	}

	if (Number(match[1]) > PAIRING_CODE_VERSION) {
		throw new WorkspaceError('The pairing code comes from a newer server', 'tooNew');
	}

	const raw = readJson(match[2]);
	const { v, id, n, p, f, o, h, s } = raw;

	if (v !== PAIRING_CODE_VERSION) throw damaged();
	if (!isNonEmptyString(id) || !isNonEmptyString(n) || !isNonEmptyString(s)) throw damaged();
	if (typeof p !== 'string' || p.length < MIN_PASSWORD_LENGTH) throw damaged();
	if (typeof o !== 'number' || !Number.isInteger(o) || o < 1 || o > 65535) throw damaged();
	if (!Array.isArray(h) || h.length === 0 || !h.every(isNonEmptyString)) throw damaged();

	return {
		serverId: id,
		name: n,
		secret: s,
		password: p,
		fingerprint: readFingerprint(f),
		port: o,
		hosts: [...h],
	};
}
