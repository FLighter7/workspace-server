import { readFile } from 'node:fs/promises';
import { hostname, networkInterfaces } from 'node:os';
import { DocumentStore } from './DocumentStore.ts';
import { ConfigError, loadConfig, resolveToken } from './config.ts';
import { createServer } from './server.ts';

const PAIRING_VERSION = 1;

/**
 * The same grammar the QR sync codes use: a leading version digit, a marker
 * for what kind of code it is, a mode, then the payload. The version digit
 * is what lets an older app say "this is newer than I understand" instead
 * of decoding it into nonsense.
 */
function pairingCode(url: string, token: string, label: string): string {
	const payload = JSON.stringify({ u: url, t: token, n: label });
	return `${PAIRING_VERSION}sj.${Buffer.from(payload, 'utf8').toString('base64url')}`;
}

function localAddresses(): string[] {
	return Object.values(networkInterfaces())
		.flat()
		.filter((entry) => entry && entry.family === 'IPv4' && !entry.internal)
		.map((entry) => entry!.address);
}

async function main(): Promise<void> {
	const config = loadConfig(process.env);
	const token = await resolveToken(process.env, config.dataRoot);

	const https = config.tls
		? { cert: await readFile(config.tls.cert), key: await readFile(config.tls.key) }
		: undefined;

	const app = createServer({
		store: new DocumentStore(config.dataRoot),
		token,
		allowedOrigins: config.allowedOrigins,
		maxBodyBytes: config.maxBodyBytes,
		https,
	});

	await app.listen({ port: config.port, host: '0.0.0.0' });

	const scheme = https ? 'https' : 'http';
	const host = process.env.PUBLIC_HOST?.trim() || `${hostname()}.local`;
	const url = `${scheme}://${host}:${config.port}`;

	console.log(`workspace-server is up, storing documents in ${config.dataRoot}`);
	console.log(`  ${url}`);
	for (const address of localAddresses()) {
		console.log(`  ${scheme}://${address}:${config.port}`);
	}

	if (!https) {
		console.log('');
		console.log('WARNING: no TLS. Only this machine can use the server —');
		console.log('a browser refuses a plain-HTTP call into the local network');
		console.log('from any page that is not itself on localhost.');
	}

	console.log('');
	console.log(`token: ${token}`);
	console.log('');
	console.log('pairing code (paste it into the app, or let it scan the QR):');
	console.log(`  ${pairingCode(url, token, hostname())}`);
	console.log('');
	console.log('Allowed origins:');
	for (const origin of config.allowedOrigins) {
		console.log(`  ${origin}`);
	}
}

main().catch((error: unknown) => {
	if (error instanceof ConfigError) {
		console.error(`configuration problem: ${error.message}`);
		process.exit(2);
	}

	console.error(error);
	process.exit(1);
});
