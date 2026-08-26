import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

export interface TlsPaths {
	cert: string;
	key: string;
}

export interface Config {
	dataRoot: string;
	port: number;
	allowedOrigins: string[];
	maxBodyBytes: number;
	/** `null` means plain HTTP — usable from localhost only, see the README. */
	tls: TlsPaths | null;
}

export class ConfigError extends Error {
	constructor(message: string) {
		super(message);
		this.name = 'ConfigError';
	}
}

const DEFAULT_PORT = 8443;
const DEFAULT_MAX_BODY_BYTES = 5 * 1024 * 1024;
const TOKEN_FILE = '.token';

/**
 * Vite's dev and preview servers, always allowed. They are the origins the
 * apps being developed against this server actually run on, and having to
 * configure them is a papercut with no security benefit — a browser will
 * not let an attacker's page claim to be localhost.
 */
const DEFAULT_ORIGINS = ['http://localhost:5173', 'http://localhost:4173'];

export function loadConfig(env: Record<string, string | undefined>): Config {
	const dataRoot = env.DATA_ROOT?.trim();
	if (!dataRoot) {
		throw new ConfigError('DATA_ROOT is required: it is the folder documents are stored in');
	}

	return {
		dataRoot,
		port: readPort(env.PORT),
		allowedOrigins: readOrigins(env.ALLOWED_ORIGINS),
		maxBodyBytes: DEFAULT_MAX_BODY_BYTES,
		tls: readTls(env),
	};
}

function readPort(value: string | undefined): number {
	if (value === undefined || value.trim() === '') {
		return DEFAULT_PORT;
	}

	const port = Number(value);
	if (!Number.isInteger(port) || port < 1 || port > 65_535) {
		throw new ConfigError(`PORT must be a whole number between 1 and 65535: ${value}`);
	}

	return port;
}

function readOrigins(value: string | undefined): string[] {
	const configured = (value ?? '')
		.split(',')
		.map((origin) => normalizeOrigin(origin.trim()))
		.filter((origin) => origin !== '');

	return [...new Set([...configured, ...DEFAULT_ORIGINS])];
}

/**
 * A browser sends `Origin` with the scheme and host lowercased and no path,
 * whatever the user typed. Comparing against the raw configured string would
 * therefore never match a host with capitals in it — and because CORS is
 * enforced in the browser, the failure arrives as a bare "load failed" that
 * looks exactly like the server being switched off.
 *
 * Anything that will not parse is kept as-is rather than dropped: silently
 * removing an entry the user meant would be a worse way to fail than not
 * matching it.
 */
function normalizeOrigin(value: string): string {
	try {
		return new URL(value).origin;
	} catch {
		return value;
	}
}

/**
 * Both halves or neither. Accepting a certificate without its key and
 * quietly falling back to HTTP would produce a server that looks configured
 * and is reachable from nothing but its own machine.
 */
function readTls(env: Record<string, string | undefined>): TlsPaths | null {
	const cert = env.TLS_CERT?.trim();
	const key = env.TLS_KEY?.trim();

	if (!cert && !key) {
		return null;
	}

	if (!cert || !key) {
		throw new ConfigError('TLS_CERT and TLS_KEY must be set together, or neither');
	}

	return { cert, key };
}

/**
 * The environment wins, so rotating a token is one restart away. Otherwise
 * the token is generated once and kept beside the data, which means the
 * paired devices keep working across restarts — a token that changed every
 * launch would unpair every device every launch.
 */
export async function resolveToken(
	env: Record<string, string | undefined>,
	dataRoot: string,
): Promise<string> {
	const configured = env.TOKEN?.trim();
	if (configured) {
		return configured;
	}

	const path = join(dataRoot, TOKEN_FILE);

	try {
		const stored = (await readFile(path, 'utf8')).trim();
		if (stored !== '') {
			return stored;
		}
	} catch {
		// Missing or unreadable: fall through and mint a new one.
	}

	const token = randomBytes(24).toString('base64url');
	await mkdir(dataRoot, { recursive: true });
	await writeFile(path, `${token}\n`, 'utf8');

	return token;
}
