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
	/** A subordinate while linked to an upstream server, else a main. */
	role: Role;
	/** The family's term: it goes up by one every time a subordinate becomes the main. */
	term: number;
}

export type Role = 'main' | 'subordinate';

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
	/** "" for a server-kind device. */
	app: string;
	/** "server": a server linked to this one, its subordinate. */
	kind?: 'server';
	name: string;
	pairedAt: string;
	lastSeen: string;
	/** A session is open now. */
	connected: boolean;
	/** What a server-kind device announced; absent until it has. */
	server?: LinkedServer;
}

/** How devices reach a server linked to this one, as it announced it. */
export interface LinkedServer {
	serverId: string;
	name: string;
	port: number;
	hosts: string[];
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

/** GET /api/upstream: the link to an upstream server (internal/upstream, Status). */
export interface UpstreamStatus {
	linked: boolean;
	/** The main's. */
	serverId?: string;
	name?: string;
	hosts?: string[];
	linkedAt?: string;
	connected: boolean;
	/** The link stopped until the server starts again; reason says why. */
	halted: boolean;
	reason?: string;
	lastError?: string;
	/** The last time a document moved between the two servers. */
	lastSync?: string;
	/** The apps synced now. */
	apps: string[];
	/** The apps the write guard stopped until the server starts again. */
	stopped?: string[];
	/** The main's devices this server admits. */
	devices?: number;
	/** The servers linked to this one: a main with any does not link. */
	subordinates: number;
	/** The family's term as this server knows it. */
	term: number;
	/** On a main that took the role over: the servers it waits for to follow it. */
	waiting?: { serverId: string; name: string }[];
	/** This server is becoming the main now. */
	promoting?: boolean;
}

/** POST /api/upstream/promote. */
export interface Promoted {
	/** The former main follows this server already; false after a forced switch. */
	followed: boolean;
}

/** POST /api/upstream/hand-over. */
export interface HandedOver {
	/** This server follows the new main already. */
	done: boolean;
}

/** DELETE /api/upstream. */
export interface Unlinked {
	/** The main no longer knows this server: it was told, or had removed it already. */
	forgotten: boolean;
}
