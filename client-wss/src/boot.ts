/** What the server writes into the page's boot block (internal/web, pageBoot). */
export interface Boot {
	/** The CSP nonce: Vuetify's theme stylesheet carries it, and only it gets in. */
	nonce: string;
	/** apps.DefaultQuota, in bytes: the add form's quota. */
	quota: number;
}

/**
 * Under `pnpm run dev` Vite serves the page with the block unfilled. These then
 * mirror the defaults in internal/apps; the server's own are the truth.
 */
export const DEV_BOOT: Readonly<Boot> = { nonce: '', quota: 52_428_800 };

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
		quota: typeof data.quota === 'number' ? data.quota : DEV_BOOT.quota,
	};
}

export const boot = readBoot();
