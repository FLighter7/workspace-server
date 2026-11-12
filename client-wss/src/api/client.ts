import type {
	AppInfo,
	ConfigChange,
	DataDirCheck,
	DataDirVerdict,
	DeviceInfo,
	HandedOver,
	NewApp,
	Pairing,
	Promoted,
	Refusal,
	Status,
	Unlinked,
	UpstreamStatus,
} from './types';

/**
 * An error of the API: the server's code ("invalid", "exists", …), or
 * "network". reason: why the upstream server refused, when code is "upstream".
 */
export class ApiError extends Error {
	constructor(
		readonly code: string,
		message: string,
		readonly reason = '',
	) {
		super(message);
		this.name = 'ApiError';
	}
}

/** The header every request carries: the server's guard refuses a write without it. */
export const MARKER_HEADER = 'X-Workspace-Server';

/**
 * Sends a request to the page's API. 201 and 204 resolve to null; any other
 * success to the parsed JSON. A failure rejects with ApiError: the server's
 * code and English message, or "network" when the server cannot be reached.
 */
export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
	const headers: Record<string, string> = { [MARKER_HEADER]: '1' };
	const init: RequestInit = { method, headers, cache: 'no-store' };
	if (body !== undefined) {
		headers['Content-Type'] = 'application/json';
		init.body = JSON.stringify(body);
	}

	let response: Response;
	try {
		response = await fetch(path, init);
	} catch {
		throw new ApiError('network', '');
	}

	if (response.status === 201 || response.status === 204) return null as T;

	let data: unknown = null;
	try {
		data = await response.json();
	} catch {
		// Not JSON.
	}

	if (!response.ok) {
		const error = (data ?? {}) as { error?: unknown; message?: unknown; reason?: unknown };
		throw new ApiError(
			typeof error.error === 'string' && error.error ? error.error : 'internal',
			typeof error.message === 'string' && error.message ? error.message : response.statusText,
			typeof error.reason === 'string' ? error.reason : '',
		);
	}

	return data as T;
}

const app = (id: string): string => `/api/apps/${encodeURIComponent(id)}`;

/** Where the app's current document downloads from. */
export const documentUrl = (id: string): string => `${app(id)}/document`;

export const serverApi = {
	status: () => request<Status>('GET', '/api/status'),
	setConfig: (change: ConfigChange) => request<null>('PUT', '/api/config', change),
	checkDataDir: (path: string) =>
		request<DataDirCheck>('GET', `/api/data-dir?path=${encodeURIComponent(path)}`),
	/** Null: path is the current folder. Otherwise the server restarts. */
	setDataDir: (dataDir: string, verdict: DataDirVerdict) =>
		request<{ restarting: boolean } | null>('PUT', '/api/config', { dataDir, verdict }),
	stop: () => request<unknown>('POST', '/api/server/stop', { removeAutostart: true }),
	reset: () => request<unknown>('DELETE', '/api/data'),
	deleteServer: () => request<unknown>('DELETE', '/api/server'),

	apps: () => request<AppInfo[]>('GET', '/api/apps'),
	addApp: (newApp: NewApp) => request<null>('POST', '/api/apps', newApp),
	deleteApp: (id: string, confirm: string) => request<null>('DELETE', app(id), { confirm }),

	devices: () => request<DeviceInfo[]>('GET', '/api/devices'),
	removeDevice: (id: string) => request<null>('DELETE', `/api/devices/${encodeURIComponent(id)}`),

	issuePairing: () => request<Pairing>('POST', '/api/pairing'),
	refusals: () => request<Refusal[]>('GET', '/api/pairing/refusals'),

	upstream: () => request<UpstreamStatus>('GET', '/api/upstream'),
	/** Answers once the pairing with the main is done, which can take a while. */
	link: (code: string) => request<UpstreamStatus>('POST', '/api/upstream', { code }),
	unlink: () => request<Unlinked>('DELETE', '/api/upstream'),
	/** A pairing code of the main, which it issues over the link. */
	invite: () => request<Pairing>('POST', '/api/upstream/invite'),
	/** Makes this server, a subordinate, the main: planned, or forced while the main is away. */
	promote: (force: boolean) => request<Promoted>('POST', '/api/upstream/promote', { force }),
	/** Asks the subordinate serverId to take the role of main over. */
	handOver: (serverId: string) =>
		request<HandedOver>('POST', '/api/upstream/hand-over', { serverId }),
};
