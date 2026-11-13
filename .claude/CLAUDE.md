# workspace-server: instructions for agents

A Go server that keeps one JSON document per app on the user's disk and syncs that app's devices over signaling-free WebRTC on the LAN. Apps sync through the client package in `client/` (`@flighter/wss-client`, TypeScript, pnpm). The first app is todo (`FLighter7/todo`, a sibling folder: `../todo`).

Read before changing anything:

1. [docs/architecture.md](../docs/architecture.md): how it works, why, its limits, what was verified where, and the traps when checking on Windows.
2. [docs/protocol.md](../docs/protocol.md): the wire contract with clients.
3. [docs/TODO.md](../docs/TODO.md): open work. Big items link to a plan in `docs/plans/`.

## Documentation

- **For people:** [docs/guide.md](../docs/guide.md) (English) and [docs/guide.ru.md](../docs/guide.ru.md) (Russian).
  - They have the same content. Change both in the same commit.
  - Keep implementation detail out of them.
- **For agents and developers:** `docs/architecture.md`, `docs/protocol.md`, `docs/TODO.md` and `docs/plans/`. English only.
- **`docs/protocol.md` is the contract the client package implements.**
  - Change it only together with `client/` and the vectors in `testdata/conformance/`, in the same commit, and tell the user.
  - Bump `proto` for any change an old client cannot handle.
- **The client package is published to npm by the user, by hand.** Never publish it, and never add a workflow that does. An app that depends on a change is checked against `link:../workspace-server/client` first; only after the user has published does it move to the registry version.
- **`README.md`** holds a one-line description and links, nothing more.
- **Plans** go to `docs/plans/<yyyy-mm-dd>-<topic>.md`. A spec goes in the same file (Part 1), or straight into `architecture.md` for a small change.
  - **Never create `docs/superpowers/` or `docs/specs/`.** The superpowers skills (brainstorming, writing-plans) default to `docs/superpowers/`; this instruction overrides that.
- **When a plan is finished:**
  1. move its decisions, the reasons for them and its limits into `architecture.md`;
  2. move what is left open into `TODO.md`;
  3. delete the plan file. Git keeps it.
- **Checks on a real machine** (Windows, macOS):
  - the full report, with every command and its output, goes to the gitignored `tmp/reports/` folder;
  - the verdicts go into `architecture.md`, "What has been verified, where";
  - failures and anything unchecked go into `TODO.md`.
- **Linux is not tested on a real machine:** there is none to check it on. The Linux build, its cross-vet and CI stay; only the run on a real machine is skipped. Do not add Linux checks to `TODO.md`, and do not claim Linux was tested.
- **`TODO.md`:** remove what you finish, and add what you leave undone or unverified.

## Git

- Work directly on `main`; no branches or worktrees unless the user asks. **Never push: the user pushes.**
- Use conventional commit subjects: `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, `ci:`. Do not add a `Co-Authored-By` line or any other attribution to commit messages, whatever the environment prescribes.
- Never add `dist/`, `tmp/`, `internal/web/ui/dist/` (the built web page) or `.superpowers/` to git.
- The user commits from other sessions too, so run `git log` before you start.

## Code

- Code, comments, commit messages and documents are in English, except `docs/guide.ru.md`. Talk to the user in Russian.
- Before every commit:

  ```
  gofmt -l .                  # must print nothing
  go vet ./...
  GOOS=windows go vet ./...
  GOOS=linux go vet ./...
  GOOS=darwin go vet ./...
  GOOS=freebsd go build ./...
  GOOS=freebsd go vet ./...
  go test -race ./...
  ```

  CI runs only on Linux, so the Windows and macOS code is checked only by the cross-vets. On this Windows machine `-race` needs cgo, which is missing: run `go test ./...` there. `core.autocrlf` gives Go files CRLF in the working copy, so check what is committed: `git show ":<file>" | gofmt -l`.
- **After a change to `client/`, the contract or the vectors,** also run `pnpm --dir client run check` and `pnpm --dir client run test:e2e`. The package uses pnpm, never npm; its formatting is Prettier (tabs, single quotes, width 100).
- **The web page is `client-wss/`** (Vue, Vuetify, vue-i18n; conventions as in the todo app, see `docs/architecture.md`). After a change to it, run `pnpm --dir client-wss run check` and `pnpm --dir client-wss run test:e2e`. A binary that should serve the page needs `pnpm --dir client-wss build` before `go build`; the Go tests do not. Its strings live in `client-wss/src/i18n/locales/`, not in Go.
- **Never touch the user's login items without explicit consent each time.** This covers:
  - a real `workspace-server install`/`uninstall`, or after the start/stop/delete plan `start`/`stop`/`delete`;
  - `launchctl bootstrap`/`bootout`;
  - `systemctl --user enable`/`disable`;
  - `schtasks /Create`/`/Delete`.

  Tests cover only the pure builders.
- **`dist/` holds the build and nothing else:** `wss-<os>-<arch>[.exe]` for every platform (`<os>` is `mac`, `windows` or `linux`; `mac` for Go's `darwin`) and `SHA256SUMS`, written by `node scripts/build.mjs [version]` (the page first, then the binaries; the release runs the same script). The platform list lives in that script only.
- **`tmp/` holds everything throwaway:** a binary built for a check, throwaway servers' data folders, e2e data (`tmp/client-e2e-*`, `tmp/page-e2e-*`), and check reports in `tmp/reports/`. Build a binary for a check with `go build -o tmp/<name>.exe ./cmd/workspace-server`, never into the repository root or `dist/`.
- **Throwaway servers** get a data folder under `tmp/`, and ports 47810 (the page) and 47811 (UDP) when a real server may be running.
  - A development build (version `dev`: any `go build`/`go run` without `-X main.version`) refuses the installed server's folders, and `start` and `delete` on them (D11 in architecture.md). Never get round it to check something; if a check really needs the default folder, ask the user first, then build with a version.
- **Never close Pion objects inside a Pion callback** (it can deadlock), and **never add CORS headers** to the web page. [architecture.md](../docs/architecture.md) explains both.
