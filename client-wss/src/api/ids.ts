/** The characters and length of an app id: internal/apps checks the same. */
export const ID_PATTERN = /^[a-z0-9-]{1,64}$/;

/** The characters an app id is made of, and the longest it can be. */
const ID_CHARS = /[a-z0-9-]/g;
export const ID_MAX_LENGTH = 64;

/** A text as an app id can hold it: in lowercase, with every character it cannot hold dropped. */
export const keepIdChars = (text: string): string =>
	text.toLowerCase().match(ID_CHARS)?.join('') ?? '';

/** Windows' reserved device names, which cannot name the app's folder: internal/fsutil, WindowsDevice. */
export const RESERVED_ID = /^(con|prn|aux|nul|com[0-9]|lpt[0-9])$/;

/** An app id the server can serve. */
export const validAppId = (id: string): boolean => ID_PATTERN.test(id) && !RESERVED_ID.test(id);
