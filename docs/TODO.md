# TODO

Open work, and what is left to check. Remove an item when it is done; add what you leave undone or unverified. How things work and why: [architecture.md](architecture.md).

## Waiting for the user

- **Publish `@flighter/wss-client`, a release candidate first,** by hand ([architecture.md](architecture.md#packaging-ci-release), "Publishing the package"; the user's decision, 2026-11-13). No version is on npm yet; the first carries fallback servers and following the main. In this order:
  - publish `1.0.0-rc.1` from the package's folder: `cd client`, then `pnpm publish --access public --tag next` (not `pnpm --dir client publish`, which npm refuses: see "Publishing the package"). Tag the server `v1.0.0-rc.1`, which `release.yml` makes a GitHub pre-release;
  - todo moves from `link:../workspace-server/client` to exactly `1.0.0-rc.1` (a range such as `^1.0.0` does not match a pre-release) and runs its checks (`pnpm test`, `pnpm run lint`, `pnpm run build`, `pnpm run test:e2e`). Until then todo's CI cannot install the link, so its GitHub Pages deploy fails, and the deployed todo is a build from 2026-10-28, from before server sync. In the same stage both guides drop the "Not available yet" note under "A laptop as a second server", since the deployed todo then has fallback servers;
  - the user checks the release candidate by hand. A bug found meanwhile is fixed and goes out the same way as `1.0.0-rc.2` (tag `v1.0.0-rc.2`, todo on exactly `1.0.0-rc.2`);
  - publish `1.0.0` from `client/` without `--tag next` (`pnpm publish --access public`), tag the server `v1.0.0`, and todo moves to `^1.0.0`.

## Checks on real machines

Everything about the local hub (a subordinate, handing it out, switching the main) has run on one Windows machine only, every server on `127.0.0.1`. The steps, with pass/fail criteria and a results table, are in `tmp/reports/2026-11-12-hub-manual-steps.md` (gitignored, on the user's machine). Verdicts go into [architecture.md](architecture.md#what-has-been-verified-where), "What has been verified, where".

- **The network away from home** ([architecture.md](architecture.md#away-from-home-the-network-not-checked-yet)): the Mac with no network at all, in Safari and in Chrome, with and without a `127.0.0.1` host in the code; a phone on its own hotspot finding the laptop by its `.local` name, iPhone and Android. The results decide whether a `127.0.0.1` host gets built.
- **The Mac as the laptop:** a foreign network, sleep and wake, the PC's server restarting.
- **Linking on two machines:** the laptop linked to the PC by a pasted code and by a real camera; "Pair this computer's browser" in Safari and in Chrome; the camera's `BarcodeDetector` path (Chrome on macOS uses it instead of qr-scanner's worker, untested anywhere).
- **Switching the main on two machines:** a planned switch from each page; a forced one with the PC off, then the PC back on and stepping down by itself; removing a PC gone for good on the laptop's Devices.
- **Phones through the laptop:** iPhone and Android on the laptop's server through a hotspot; a phone paired with the PC syncing through the laptop while the PC is off, and following the laptop after a switch. These need a todo with fallback servers on the phone: the deployed one once todo is on `1.0.0-rc.1`, or a local build behind an HTTPS tunnel.
- **Clients never tried at all:** Mac Safari and Android (the hub checks above cover both).
- **todo's screen reader run** (`pnpm run test:sr`, Windows, the machine left alone), not run since the Sync page gained the "Through {name}…" line, the `notMain` reason, and "Server: {name}" after the device follows a new main. No NVDA scenario covers those lines yet: the run catches regressions, and the new lines need a scenario or a listen by hand.
- **The diagram** (`docs/images/connections*.svg`) was rendered and checked only on Windows (Chromium and WebKit, with glyphs 7% wider standing in for Mac fonts): look at it on GitHub, light and dark theme, and in Safari on the Mac.
- **Older, unchecked:** the mDNS responder on a Windows machine with several adapters; whether a device connects right after "Allow" in the Windows Firewall dialog of a first start.

## Known gaps

- **`forget` through a subordinate does not reach the main** (L20): the main keeps the device until it is removed on the main's page. Relaying it needs a new frame.
- **The device registry travels in one frame** of at most 256 KiB: about 700 devices with a full list of 1,000 removals. A family near that size needs the registry split over several frames.
- **A server removed on one side during a split** cannot be told by the other (L29); two mains then stay mains until the user unlinks or links one by hand.
- **A promoted main dials the servers it waits for** up to every 5 minutes until each has followed it once (L30), a laptop asleep included.
- **An `apps` answer can arrive after a newer `apps` push** on a link session, so a subordinate may sync an older app list until the next change or reconnect. Rare; answering under the same lock as the pushes, as the registry does, would close it.
- **The main's page keeps showing a code an `invite` revoked** until it expires (L21); pairing with it then gets `badSecret`.
- **The Upstream server section on a promoted main is wordy;** tighten its text.
- **Older servers** (moot until releases differ, since none of these versions was released): a main from before `invite` answers it with `protocol`, which closes the link session, and the subordinate opens it again (untested); a subordinate from before the switch of the main would halt on `takeOver`.
- **Docker:** an image for a NAS is a later step; it needs host networking for UDP and mDNS.
