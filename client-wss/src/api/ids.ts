/** The characters and length of an app id: internal/apps checks the same. */
export const ID_PATTERN = /^[a-z0-9-]{1,64}$/;

/** Windows' reserved device names, which cannot name the app's folder: internal/fsutil, WindowsDevice. */
export const RESERVED_ID = /^(con|prn|aux|nul|com[0-9]|lpt[0-9])$/;

/** An app id the server can serve. */
export const validAppId = (id: string): boolean => ID_PATTERN.test(id) && !RESERVED_ID.test(id);
