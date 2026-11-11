// Builds the release: the web page, then the server for every platform, into
// a clean dist/ with SHA256SUMS. The release workflow runs this same script.
//
//   node scripts/build.mjs [version]
//
// version goes into `workspace-server version`; it defaults to "dev".

import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

/** Every platform a release carries: GOOS, GOARCH. */
const TARGETS = [
	['darwin', 'arm64'],
	['darwin', 'amd64'],
	['windows', 'amd64'],
	['linux', 'amd64'],
	['linux', 'arm64'],
];

const ROOT = resolve(import.meta.dirname, '..');
const DIST = join(ROOT, 'dist');
const version = process.argv[2] ?? 'dev';

function run(command, args, env = {}) {
	console.log(`> ${command} ${args.join(' ')}`);
	// pnpm is a .cmd file on Windows, which only cmd.exe starts.
	const [file, argv] =
		process.platform === 'win32' && command === 'pnpm'
			? ['cmd.exe', ['/d', '/s', '/c', command, ...args]]
			: [command, args];
	const result = spawnSync(file, argv, {
		cwd: ROOT,
		stdio: 'inherit',
		env: { ...process.env, ...env },
	});
	if (result.status !== 0) {
		console.error(`${command} failed${result.error ? `: ${result.error.message}` : ''}`);
		process.exit(1);
	}
}

// The page, which every binary embeds.
run('pnpm', ['--dir', 'client-wss', 'install', '--frozen-lockfile']);
run('pnpm', ['--dir', 'client-wss', 'run', 'build']);
if (!existsSync(join(ROOT, 'internal', 'web', 'ui', 'dist', 'index.html'))) {
	console.error('the page was not built: internal/web/ui/dist/index.html is missing');
	process.exit(1);
}

rmSync(DIST, { recursive: true, force: true });
mkdirSync(DIST);

const files = [];
for (const [goos, goarch] of TARGETS) {
	const name = `wss-${goos}-${goarch}${goos === 'windows' ? '.exe' : ''}`;
	run(
		'go',
		[
			'build',
			'-trimpath',
			'-ldflags',
			`-s -w -X main.version=${version}`,
			'-o',
			join(DIST, name),
			'./cmd/workspace-server',
		],
		{ CGO_ENABLED: '0', GOOS: goos, GOARCH: goarch },
	);
	files.push(name);
}

// The format of sha256sum, so that `sha256sum -c SHA256SUMS` checks it.
const sums = files
	.map((name) => {
		const hash = createHash('sha256')
			.update(readFileSync(join(DIST, name)))
			.digest('hex');
		return `${hash}  ${name}\n`;
	})
	.join('');
writeFileSync(join(DIST, 'SHA256SUMS'), sums);

console.log(`\n${version}, in dist/:\n${sums}`);
