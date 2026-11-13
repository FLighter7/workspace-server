/**
 * Test doubles for apps that use `@flighter/wss-client`: a data
 * channel stand-in for `ServerClient`, and an in-memory server for
 * `SyncEngine`.
 */
export { FakeChannel } from './FakeChannel.js';
export { FakeClient, FakeServer } from './FakeServer.js';
