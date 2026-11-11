// Force module mode so the top-level `await import(...)` below is legal: TypeScript treats a
// file with zero static import/export statements as a script, where top-level await is not
// allowed. This export is a no-op at runtime.
export {};

if (typeof window !== 'undefined') {
	class ResizeObserverStub {
		observe(): void {}
		unobserve(): void {}
		disconnect(): void {}
	}

	window.ResizeObserver ??= ResizeObserverStub as unknown as typeof ResizeObserver;

	window.matchMedia ??= (query: string): MediaQueryList =>
		({
			matches: false,
			media: query,
			onchange: null,
			addListener: () => {},
			removeListener: () => {},
			addEventListener: () => {},
			removeEventListener: () => {},
			dispatchEvent: () => false,
		}) as MediaQueryList;

	// Vuetify's VOverlay (used by v-dialog) accesses visualViewport to calculate positioning.
	// happy-dom doesn't define it, so we stub it following the pattern above.
	if (!window.visualViewport) {
		Object.defineProperty(window, 'visualViewport', {
			value: {
				width: 1024,
				height: 768,
				offsetLeft: 0,
				offsetTop: 0,
				pageLeft: 0,
				pageTop: 0,
				scale: 1,
				addEventListener: () => {},
				removeEventListener: () => {},
			},
			configurable: true,
		});
	}

	if (!window.localStorage || typeof window.localStorage.getItem !== 'function') {
		const store: Record<string, string> = {};
		window.localStorage = {
			getItem: (key: string) => store[key] ?? null,
			setItem: (key: string, value: string) => {
				store[key] = value;
			},
			removeItem: (key: string) => {
				delete store[key];
			},
			clear: () => {
				for (const key of Object.keys(store)) {
					delete store[key];
				}
			},
			key: (index: number) => Object.keys(store)[index] ?? null,
			length: 0,
		};
	}

	// @vue/test-utils finds only the mounted component's own subtree, and
	// VDialog teleports its content to document.body: render it in place.
	const testVuetify = (await import('@/plugins/vuetify')).default;
	testVuetify.defaults.value = {
		...(testVuetify.defaults.value ?? {}),
		VDialog: { attach: true },
	};
}
