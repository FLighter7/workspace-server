import { mdiCellphone, mdiMonitor, mdiTablet } from '@mdi/js';

/**
 * The icon for what kind of device a name tells: a device names itself by its
 * system and browser when it pairs ("iPhone Safari", "Windows Chrome (PWA)").
 */
export function deviceKindIcon(name: string): string {
	if (/iPhone|Android/.test(name)) return mdiCellphone;
	if (/iPad/.test(name)) return mdiTablet;
	return mdiMonitor;
}
