import type { AppData } from '@/api/types';
import type { MessageKey } from '@/i18n';

/** An app's data: from the data-folder check (no name, no date) or the app list. */
export type AppFactItem = AppData & { name?: string; modified?: string | null };

/** One fact in the wipe dialog: what the action moves or deletes. */
export type WipeFact =
	| { kind: 'apps'; apps: AppFactItem[]; download: boolean }
	| { kind: 'path'; label: MessageKey; path: string }
	| { kind: 'count'; label: MessageKey; value: number };

/** A question about data, with the facts it acts on. */
export interface Wipe {
	title: MessageKey;
	text: MessageKey;
	ok: MessageKey;
	/** Deletes for good: the OK button is red. */
	danger: boolean;
	facts: WipeFact[];
	/** Runs on OK. The dialog stays open while it runs, shows its error, closes on success. */
	action: () => Promise<void>;
}
