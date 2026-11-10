/** The part of `SyncEngine` that follows the page. */
export interface PageAware {
	start(visible: boolean): void;
	setVisible(visible: boolean): void;
	online(): void;
}

/** The page's window and document, or stand-ins for them in a test. */
export interface Page {
	window: Pick<Window, 'addEventListener' | 'removeEventListener'>;
	document: Pick<Document, 'addEventListener' | 'removeEventListener' | 'visibilityState'>;
}

/**
 * Starts the engine and keeps telling it whether the page is on screen and
 * whether the network came back: the connection is open only while the app
 * is visible. Returns a function that stops listening.
 */
export function startInPage(engine: PageAware, page: Page = { window, document }): () => void {
	const visible = (): boolean => page.document.visibilityState === 'visible';
	const onVisibility = (): void => engine.setVisible(visible());
	// A page restored from the back-forward cache fires no visibilitychange.
	const onPageShow = (event: Event): void => {
		if ((event as PageTransitionEvent).persisted) engine.setVisible(true);
	};
	const onOnline = (): void => engine.online();

	page.document.addEventListener('visibilitychange', onVisibility);
	page.window.addEventListener('pageshow', onPageShow);
	page.window.addEventListener('online', onOnline);
	engine.start(visible());

	return () => {
		page.document.removeEventListener('visibilitychange', onVisibility);
		page.window.removeEventListener('pageshow', onPageShow);
		page.window.removeEventListener('online', onOnline);
	};
}
