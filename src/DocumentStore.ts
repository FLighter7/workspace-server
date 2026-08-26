import { createHash } from 'node:crypto';
import { open, mkdir, readFile, rename, unlink } from 'node:fs/promises';
import { join } from 'node:path';
import { assertValidName } from './names.ts';

export interface StoredDocument {
	body: string;
	/** Quoted, the way an HTTP ETag header carries it. */
	etag: string;
}

export class PreconditionFailedError extends Error {
	constructor(message: string) {
		super(message);
		this.name = 'PreconditionFailedError';
	}
}

function etagOf(body: string): string {
	return `"${createHash('sha256').update(body, 'utf8').digest('hex')}"`;
}

/**
 * Documents on disk, one JSON file each, with no database and no schema.
 * The store never looks inside a body: what an app stores is the app's
 * business, and staying incurious is what lets one server serve several.
 *
 * Concurrency is handled with an optimistic etag rather than a lock held
 * across a round trip: a client reads, merges, and writes back the version
 * it saw, and a write that lost the race is refused rather than applied on
 * top of someone else's.
 */
export class DocumentStore {
	private readonly root: string;
	/**
	 * One promise chain per document, so a read-then-rename inside `write`
	 * cannot interleave with another write to the same file. Keyed by path,
	 * so unrelated documents still write in parallel.
	 */
	private readonly queues = new Map<string, Promise<unknown>>();

	constructor(root: string) {
		this.root = root;
	}

	async read(app: string, doc: string): Promise<StoredDocument | null> {
		const path = this.pathFor(app, doc);

		try {
			const body = await readFile(path, 'utf8');
			return { body, etag: etagOf(body) };
		} catch (error) {
			if (isMissing(error)) {
				return null;
			}

			throw error;
		}
	}

	/**
	 * `expected` is the etag the caller believes is current, or `null` to mean
	 * "I believe there is nothing here yet" — the two spellings of `If-Match`
	 * and `If-None-Match: *`. There is deliberately no unconditional write:
	 * every caller either read an etag or got a 404, so every caller can say
	 * which one it means, and an accidental clobber becomes impossible to
	 * express rather than merely discouraged.
	 */
	async write(
		app: string,
		doc: string,
		body: string,
		expected: string | null,
	): Promise<{ etag: string }> {
		const path = this.pathFor(app, doc);

		return this.enqueue(path, async () => {
			const current = await this.read(app, doc);

			if (expected === null && current !== null) {
				throw new PreconditionFailedError('the document already exists');
			}

			if (expected !== null && current === null) {
				throw new PreconditionFailedError('the document does not exist yet');
			}

			if (expected !== null && current !== null && expected !== current.etag) {
				throw new PreconditionFailedError('the document changed since it was read');
			}

			await mkdir(join(this.root, app), { recursive: true });
			await writeAtomically(path, body);

			return { etag: etagOf(body) };
		});
	}

	private pathFor(app: string, doc: string): string {
		assertValidName(app, 'the app name');
		assertValidName(doc, 'the document name');

		return join(this.root, app, `${doc}.json`);
	}

	private enqueue<T>(key: string, work: () => Promise<T>): Promise<T> {
		// Chain off whatever is pending, ignoring whether it succeeded: a
		// failed write must not stall every write that comes after it.
		const previous = this.queues.get(key) ?? Promise.resolve();
		const next = previous.then(work, work);

		this.queues.set(
			key,
			next.catch(() => undefined),
		);

		return next;
	}
}

/**
 * Write to a sibling temporary file, flush it, then rename over the target.
 * A rename within one directory is atomic, so a reader sees either the old
 * file or the new one. Writing in place would leave a truncated document
 * behind if the machine lost power mid-write — and that document is every
 * record the app has.
 */
async function writeAtomically(path: string, body: string): Promise<void> {
	const temporary = `${path}.${process.pid}.${Date.now()}.tmp`;
	const handle = await open(temporary, 'w');

	try {
		await handle.writeFile(body, 'utf8');
		await handle.sync();
	} finally {
		await handle.close();
	}

	try {
		await rename(temporary, path);
	} catch (error) {
		await unlink(temporary).catch(() => undefined);
		throw error;
	}
}

function isMissing(error: unknown): boolean {
	return (
		typeof error === 'object' &&
		error !== null &&
		'code' in error &&
		(error.code === 'ENOENT' || error.code === 'ENOTDIR')
	);
}
