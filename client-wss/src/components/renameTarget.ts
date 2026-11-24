/** What RenameDialog names. */
export interface RenameTarget {
	name: string;
	/** Shown under the field, all the time. */
	hint?: string;
	rename: (name: string) => Promise<unknown>;
}
