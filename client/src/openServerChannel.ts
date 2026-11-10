import type { ServerTarget } from './PairingCode.js';
import { WorkspaceError } from './WorkspaceError.js';
import { buildServerAnswer, randomIceUfrag } from './serverAnswer.js';

const CHANNEL_LABEL = 'workspace';
export const CONNECT_TIMEOUT_MS = 8000;

/** Connection states after which the channel will never carry anything again. */
const DEAD_STATES: ReadonlySet<RTCPeerConnectionState> = new Set([
	'failed',
	'disconnected',
	'closed',
]);

/**
 * Opens a data channel to the workspace-server with no signaling: this
 * device makes its offer and sets an answer it builds itself from the
 * pairing (see `buildServerAnswer`). Resolves once the channel is open;
 * rejects with `connectTimeout` if it does not open in time.
 *
 * Whatever ends the connection — the server going away, the network
 * changing, the app coming back from the background to a dead connection —
 * closes the channel, so the one close event is all a caller watches; and a
 * closed channel closes the peer connection, which would otherwise linger.
 */
export async function openServerChannel(
	target: ServerTarget,
	timeoutMs = CONNECT_TIMEOUT_MS,
): Promise<{ channel: RTCDataChannel; close: () => void }> {
	// No STUN or TURN: the server is on the same network, and an ICE server
	// would be an internet dependency this app does not have.
	const peer = new RTCPeerConnection({ iceServers: [] });
	const channel = peer.createDataChannel(CHANNEL_LABEL);
	channel.binaryType = 'arraybuffer';

	const close = (): void => {
		channel.close();
		peer.close();
	};

	peer.addEventListener('connectionstatechange', () => {
		if (DEAD_STATES.has(peer.connectionState)) close();
	});
	channel.addEventListener('close', () => peer.close());

	const opened = new Promise<void>((resolve, reject) => {
		const timer = setTimeout(() => {
			close();
			reject(new WorkspaceError('The server did not answer in time', 'connectTimeout'));
		}, timeoutMs);

		channel.addEventListener('open', () => {
			clearTimeout(timer);
			resolve();
		});
		channel.addEventListener('close', () => {
			clearTimeout(timer);
			reject(new WorkspaceError('The connection to the server closed', 'closed'));
		});
	});

	// Awaited below; this only keeps an early failure from also reporting the
	// rejection as unhandled.
	opened.catch(() => undefined);

	try {
		const offer = await peer.createOffer();
		// No waiting for ICE gathering: the server learns this device's address
		// from its first binding request.
		await peer.setLocalDescription(offer);
		await peer.setRemoteDescription({
			type: 'answer',
			sdp: buildServerAnswer(offer.sdp ?? '', target, randomIceUfrag()),
		});
		await opened;
	} catch (error) {
		close();
		throw error instanceof WorkspaceError
			? error
			: new WorkspaceError('Could not set up the connection to the server', 'closed');
	}

	return { channel, close };
}
