function deviceKind(userAgent: string): string {
	if (/iPhone/.test(userAgent)) return 'iPhone';
	if (/iPad/.test(userAgent)) return 'iPad';
	if (/Android/.test(userAgent)) return 'Android';
	if (/Macintosh|Mac OS X/.test(userAgent)) return 'Mac';
	if (/Windows/.test(userAgent)) return 'Windows';
	if (/Linux/.test(userAgent)) return 'Linux';

	return 'Device';
}

function browserKind(userAgent: string): string {
	// Order matters: Edge's agent also names Chrome and Safari, Chrome's names Safari.
	if (/Edg\//.test(userAgent)) return 'Edge';
	if (/Firefox|FxiOS/.test(userAgent)) return 'Firefox';
	if (/Chrome|CriOS/.test(userAgent)) return 'Chrome';
	if (/Safari/.test(userAgent)) return 'Safari';

	return 'Browser';
}

/**
 * The name this device gives itself when it pairs, shown on the server's
 * page so the user can tell their devices apart: "iPhone Safari", or
 * "iPhone Safari (app)" when installed to the Home Screen.
 */
export function deviceName(userAgent: string, standalone: boolean): string {
	const name = `${deviceKind(userAgent)} ${browserKind(userAgent)}`;

	return standalone ? `${name} (app)` : name;
}

/** Whether the page runs as an installed app: a PWA window, or added to the iOS Home Screen. */
function isStandalone(): boolean {
	const iosStandalone = (navigator as Navigator & { standalone?: boolean }).standalone === true;

	return window.matchMedia('(display-mode: standalone)').matches || iosStandalone;
}

/** `deviceName` for the browser this runs in. */
export function browserDeviceName(): string {
	return deviceName(navigator.userAgent, isStandalone());
}
