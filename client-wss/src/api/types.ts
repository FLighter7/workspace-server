// The JSON the server's API answers with: internal/web/web.go and api.go.

/** GET /api/status: what the Server section shows. */
export interface Status {
	name: string;
	serverId: string;
	mdnsName: string;
	mdnsRunning: boolean;
	/** The UDP port is open; false: devices cannot connect. */
	listening: boolean;
	addresses: string[];
	udpPort: number;
	httpPort: number;
	program: string;
	serverDir: string;
	dataDir: string;
	/** The default server folder: it owns autostart and the program file. */
	mainServer: boolean;
	/** Why the last reset or data move failed; "" when none did. */
	lastError: string;
}

/** PUT /api/config: the fields to change. */
export interface ConfigChange {
	name?: string;
	udpPort?: number;
	httpPort?: number;
}

export type DataDirVerdict =
	'same' | 'create' | 'move' | 'switch' | 'adopt' | 'conflict' | 'unavailable';

/** An app's data in a data folder. */
export interface AppData {
	id: string;
	size: number;
	versions: number;
}

/** GET /api/data-dir: what changing the data folder to path would do. */
export interface DataDirCheck {
	path: string;
	verdict: DataDirVerdict;
	message: string;
	/** The apps' data in the current folder. */
	current: AppData[];
	/** The apps' data already in path. */
	target: AppData[];
}

/** GET /api/apps: a served app and the state of its data. */
export interface AppInfo {
	id: string;
	name: string;
	history: number;
	maxBytes: number;
	/** Bytes of the current document; 0 when there is none. */
	size: number;
	/** The last write; null when there is no document. */
	modified: string | null;
	versions: number;
	devices: number;
}

/** POST /api/apps. */
export interface NewApp {
	id: string;
	name?: string;
	history?: number;
	maxBytes?: number;
}

/** GET /api/devices: one paired device. */
export interface DeviceInfo {
	id: string;
	app: string;
	name: string;
	pairedAt: string;
	lastSeen: string;
	/** A session is open now. */
	connected: boolean;
}

/** POST /api/pairing. */
export interface Pairing {
	code: string;
	expiresAt: string;
	/** A data: URL of a PNG. */
	qr: string;
}

/** GET /api/pairing/refusals: a pairing refused for an app the server does not serve. */
export interface Refusal {
	app: string;
	device: string;
	at: string;
}
