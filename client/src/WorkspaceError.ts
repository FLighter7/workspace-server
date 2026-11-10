/** The codes the server sends in an error frame (`docs/protocol.md`, "Errors"). */
export const SERVER_ERROR_CODES = [
	'protocol',
	'unknownApp',
	'badSecret',
	'badToken',
	'blocked',
	'tooLarge',
	'notJson',
	'notAuthenticated',
	'internal',
] as const;

export type ServerErrorCode = (typeof SERVER_ERROR_CODES)[number];

/** Why a pairing code was refused before anything was sent. */
export type PairingCodeErrorCode = 'notPairingCode' | 'tooNew' | 'damaged';

/** The connection itself failed: the server did not answer, or the channel closed. */
export type TransportErrorCode = 'connectTimeout' | 'closed';

/**
 * What the sync engine decides on its own: a different server answered at the
 * paired address, writes are coming too fast, or another device kept writing
 * first.
 */
export type EngineErrorCode = 'serverChanged' | 'tooManyWrites' | 'conflict';

/** Every failure this package reports. An app adds its own through `DocumentError`. */
export type WorkspaceErrorCode =
	ServerErrorCode | PairingCodeErrorCode | TransportErrorCode | EngineErrorCode;

/** The message is English, for a developer; `code` is what an interface translates. */
export class WorkspaceError<Code extends string = WorkspaceErrorCode> extends Error {
	constructor(
		message: string,
		readonly code: Code,
	) {
		super(message);
		this.name = 'WorkspaceError';
	}
}

/**
 * An app's own refusal of the server's document: a newer format than this
 * build reads, or one that does not parse. Throwing it from a `SyncAdapter`
 * halts sync until the app starts again, and nothing is written over the
 * document; the code is the app's to name and translate.
 */
export class DocumentError<Code extends string = string> extends WorkspaceError<Code> {
	constructor(message: string, code: Code) {
		super(message, code);
		this.name = 'DocumentError';
	}
}

const SERVER_CODES: ReadonlySet<string> = new Set(SERVER_ERROR_CODES);

/** Whether `code` is one the server sends; any other in an error frame reads as `internal`. */
export function isServerErrorCode(code: unknown): code is ServerErrorCode {
	return typeof code === 'string' && SERVER_CODES.has(code);
}
