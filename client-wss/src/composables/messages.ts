import { ApiError } from '@/api/client';
import type { MessageKey } from '@/i18n';

/**
 * A line in a status area. Kept as a key, not a text, so it follows a language
 * switch while it is on screen.
 */
export interface Message {
	key: MessageKey;
	/** Values for the key's {placeholders}, shown isolated: they come from elsewhere. */
	values?: Record<string, string>;
	/** Links for the key's {placeholders}, each showing its own address. */
	links?: Record<string, string>;
	/** The server's detail, in English, after the text in parentheses. */
	detail?: string;
	error?: boolean;
}

const ERROR_KEYS: Record<string, MessageKey> = {
	invalid: 'errInvalid',
	exists: 'errExists',
	confirmMismatch: 'errConfirmMismatch',
	notFound: 'errNotFound',
	forbidden: 'errForbidden',
	network: 'errNetwork',
	dataExists: 'errDataExists',
	changed: 'errDataChanged',
	busy: 'errBusy',
	notMain: 'errNotMain',
};

/** The page's text for an error: by its code, or the generic one. */
export function errorKey(error: unknown): MessageKey {
	return (error instanceof ApiError && ERROR_KEYS[error.code]) || 'errInternal';
}

/** An error as a message: the page's text, then the server's detail, which says what was refused. */
export function errorMessage(error: unknown): Message {
	return {
		key: errorKey(error),
		detail: error instanceof Error ? error.message : '',
		error: true,
	};
}

export const info = (key: MessageKey): Message => ({ key });
