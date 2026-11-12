import { ApiError } from '@/api/client';
import type { MessageKey } from '@/i18n';
import { errorMessage, type Message } from './messages';

/** Why the main refused, or could not be reached: the reason of a 502 "upstream". */
const REASON_KEYS: Record<string, MessageKey> = {
	badSecret: 'linkBadSecret',
	blocked: 'linkBlocked',
	notMain: 'linkNotMain',
	serverChanged: 'linkServerChanged',
	connectTimeout: 'linkUnreachable',
	closed: 'linkUnreachable',
	unreachable: 'linkUnreachable',
	// A main from before server-kind pairing, or before invite.
	unknownApp: 'linkTooOld',
	protocol: 'linkTooOld',
	badToken: 'haltedRemoved',
};

/** The page API's own refusals of linking, unlinking and invite. */
const CODE_KEYS: Record<string, MessageKey> = {
	invalid: 'linkInvalidCode',
	exists: 'linkExists',
	busy: 'linkBusy',
	notLinked: 'linkNotLinked',
	hasSubordinates: 'linkHasSubordinates',
};

/**
 * An error of linking, unlinking or invite as a message: the page's text for
 * the main's reason or the API's code, then the server's detail. overrides
 * replaces the text of a reason where the action needs its own words.
 */
export function upstreamError(
	error: unknown,
	overrides: Partial<Record<string, MessageKey>> = {},
): Message {
	if (error instanceof ApiError) {
		const key =
			error.code === 'upstream'
				? (overrides[error.reason] ?? REASON_KEYS[error.reason])
				: CODE_KEYS[error.code];
		if (key) return { key, detail: error.message, error: true };
	}

	return errorMessage(error);
}

/** What a halted link's reason means for the user, and what to do. */
export function haltedKey(reason: string | undefined): MessageKey {
	switch (reason) {
		case 'badToken':
			return 'haltedRemoved';
		case 'notMain':
			return 'haltedNotMain';
		case 'serverChanged':
			return 'haltedServerChanged';
		case 'protocol':
		case 'unknownApp':
			return 'haltedTooOld';
		default:
			return 'haltedOther';
	}
}
