/** What the server writes into the page's boot block (internal/web, pageBoot). */
export interface Boot {
	/** The CSP nonce: Vuetify's theme stylesheet carries it, and only it gets in. */
	nonce: string;
	/** apps.DefaultHistory: the add form's versions to keep. */
	history: number;
	/** apps.DefaultMaxBytes: the add form's largest document. */
	maxBytes: number;
}

/**
 * Under `pnpm run dev` Vite serves the page with the block unfilled. These then
 * mirror the defaults in internal/apps; the server's own are the truth.
 */
export const DEV_BOOT: Readonly<Boot> = { nonce: '', history: 10, maxBytes: 67_108_864 };

export function readBoot(doc: Document = document): Boot {
	let data: Partial<Boot> = {};
	try {
		const parsed: unknown = JSON.parse(doc.getElementById('boot')?.textContent ?? '');
		if (parsed !== null && typeof parsed === 'object') data = parsed as Partial<Boot>;
	} catch {
		// The template marker itself, under the dev server.
	}

	return {
		nonce: typeof data.nonce === 'string' ? data.nonce : DEV_BOOT.nonce,
		history: typeof data.history === 'number' ? data.history : DEV_BOOT.history,
		maxBytes: typeof data.maxBytes === 'number' ? data.maxBytes : DEV_BOOT.maxBytes,
	};
}

export const boot = readBoot();
