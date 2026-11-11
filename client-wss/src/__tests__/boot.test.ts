// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest';
import { DEV_BOOT, readBoot } from '@/boot';

function page(block: string | null): Document {
	const doc = document.implementation.createHTMLDocument();
	if (block !== null) {
		const script = doc.createElement('script');
		script.type = 'application/json';
		script.id = 'boot';
		script.textContent = block;
		doc.head.append(script);
	}
	return doc;
}

describe('readBoot', () => {
	it('reads what the server wrote', () => {
		expect(readBoot(page('{"nonce":"abc","history":5,"maxBytes":2048}'))).toEqual({
			nonce: 'abc',
			history: 5,
			maxBytes: 2048,
		});
	});

	it("falls back to the dev defaults under Vite's own server", () => {
		expect(readBoot(page('{{.Boot}}'))).toEqual(DEV_BOOT);
		expect(readBoot(page(null))).toEqual(DEV_BOOT);
	});

	it('takes only fields of the right type', () => {
		expect(readBoot(page('{"nonce":1,"history":"5","maxBytes":2048}'))).toEqual({
			...DEV_BOOT,
			maxBytes: 2048,
		});
	});
});
