/**
 * The client for workspace-server: pair a device, reach the server over
 * WebRTC on the local network with no signaling, and keep one app's
 * document in sync. The wire contract is the server's `docs/protocol.md`.
 *
 * Most apps need `parsePairingCode`, `createPairingStore`, `SyncEngine` with
 * their own `SyncAdapter`, and `startInPage`; the rest is there for apps
 * that drive the protocol themselves.
 */
export {
	BUFFER_HIGH_BYTES,
	BUFFER_LOW_BYTES,
	ChannelFraming,
	MAX_MESSAGE_BYTES,
	type Frame,
	type FrameChannel,
	type Incoming,
} from './ChannelFraming.js';
export { browserDeviceName, deviceName } from './deviceName.js';
export { CONNECT_TIMEOUT_MS, openServerChannel } from './openServerChannel.js';
export {
	PAIRING_CODE_VERSION,
	parsePairingCode,
	type PairingCode,
	type ServerTarget,
} from './PairingCode.js';
export {
	createPairingStore,
	type Pairing,
	type PairingStorage,
	type PairingStore,
	type PairingStoreOptions,
} from './PairingStore.js';
export { buildServerAnswer, randomIceUfrag } from './serverAnswer.js';
export {
	PROTOCOL_VERSION,
	ServerClient,
	type PutResult,
	type ServerClientOptions,
	type Welcome,
} from './ServerClient.js';
export { startInPage, type Page, type PageAware } from './startInPage.js';
export {
	BLOCKED_RETRY_MS,
	CHANGE_DEBOUNCE_MS,
	MAX_CONFLICT_RETRIES,
	MAX_WRITES_PER_MINUTE,
	RETRY_DELAYS_MS,
	SyncEngine,
	connectToServer,
	type ServerDocument,
	type SyncAdapter,
	type SyncClient,
	type SyncEngineOptions,
	type SyncPlan,
	type SyncState,
	type SyncStatus,
	type SyncTimers,
} from './SyncEngine.js';
export {
	DocumentError,
	SERVER_ERROR_CODES,
	WorkspaceError,
	isServerErrorCode,
	type EngineErrorCode,
	type PairingCodeErrorCode,
	type ServerErrorCode,
	type TransportErrorCode,
	type WorkspaceErrorCode,
} from './WorkspaceError.js';
