/**
 * App and document names become path segments, so they are checked against
 * an allowlist rather than scrubbed of anything dangerous. A blocklist has
 * to anticipate every spelling of "go up a directory" — this pattern cannot
 * express one at all.
 */
const NAME_PATTERN = /^[a-z0-9-]{1,64}$/;

export class InvalidNameError extends Error {
	constructor(what: string, value: string) {
		super(`${what} must be 1-64 characters of a-z, 0-9 and dashes: ${JSON.stringify(value)}`);
		this.name = 'InvalidNameError';
	}
}

export function assertValidName(value: string, what: string): void {
	if (!NAME_PATTERN.test(value)) {
		throw new InvalidNameError(what, value);
	}
}
