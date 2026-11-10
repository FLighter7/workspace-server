import { describe, expect, it, vi } from 'vitest';
import { startInPage, type Page } from '../src/startInPage.js';

function setup(visibility: DocumentVisibilityState) {
	const window = new EventTarget();
	const document = Object.assign(new EventTarget(), { visibilityState: visibility });
	const engine = { start: vi.fn(), setVisible: vi.fn(), online: vi.fn() };
	const stop = startInPage(engine, { window, document } as unknown as Page);

	return { window, document, engine, stop };
}

function pageshow(persisted: boolean): Event {
	return Object.assign(new Event('pageshow'), { persisted });
}

describe('startInPage', () => {
	it('starts the engine with the page visibility', () => {
		expect(setup('visible').engine.start).toHaveBeenCalledWith(true);
		expect(setup('hidden').engine.start).toHaveBeenCalledWith(false);
	});

	it('follows visibility, a page restored from the cache, and the network', () => {
		const { window, document, engine } = setup('visible');

		document.visibilityState = 'hidden';
		document.dispatchEvent(new Event('visibilitychange'));
		expect(engine.setVisible).toHaveBeenLastCalledWith(false);

		window.dispatchEvent(pageshow(false));
		expect(engine.setVisible).toHaveBeenCalledTimes(1);
		window.dispatchEvent(pageshow(true));
		expect(engine.setVisible).toHaveBeenLastCalledWith(true);

		window.dispatchEvent(new Event('online'));
		expect(engine.online).toHaveBeenCalledTimes(1);
	});

	it('stops listening', () => {
		const { window, document, engine, stop } = setup('visible');

		stop();
		document.dispatchEvent(new Event('visibilitychange'));
		window.dispatchEvent(pageshow(true));
		window.dispatchEvent(new Event('online'));

		expect(engine.setVisible).not.toHaveBeenCalled();
		expect(engine.online).not.toHaveBeenCalled();
	});
});
