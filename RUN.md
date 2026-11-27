# Verification and release venues

Run the two parts of `make verify` at the same commit. Keep the commit,
tree, venue, tool versions and original output in each receipt.

| Venue | Command | Scope |
| --- | --- | --- |
| Admitted Linux ARM64 production-03 or production-04 | `make verify-go` | Build, test and vet the root Go module. Check roster handling with disposable fixtures and build the client binaries. |
| Admitted Linux ARM64 production-03 or production-04 | `make verify-harness` | Run the harness integration checks (native stdio and HTTP MCP, the Python client) against a daemon and client built from this checkout. Needs the Python MCP SDK; see the Makefile comment. |
| ENG-UK | `make verify-web` | Build a local daemon and client fixtures, install the locked web dependencies, build and typecheck the frontend, run the HTTP tests against the daemon, and run the browser checks in Google Chrome. |

Plain `make verify` selects the applicable command on the approved host.
The full gate requires both receipts. The Architect runs ENG-UK jobs.
Use the assigned toolchains under `/opt/zeo/runtimes` on ENG-UK.

```sh
CHROME_PATH='/Applications/Google Chrome.app' make verify-web
```

`CHROME_PATH` defaults to `/Applications/Google Chrome.app`.
`npm ci` installs Playwright Core at the version and integrity in the lockfile.
The browser journey uses the installed Chrome and the daemon and client built
from this checkout. It starts the daemon with private fixture state on a fresh
loopback port and checks the instance in its health response before opening Chrome.
The HTTP tests receive the built daemon path through `INBOX_DAEMON`.

If the browser journey fails after opening a page, retain
`build/browser-failure.png` alongside the run log. Set
`INBOX_BROWSER_FAILURE_SCREENSHOT` to direct it to a different path.

## Release artifacts

`make release` builds the artifacts on ENG-UK into a new directory `build/release-COMMIT`: the
installer `openagentinbox-install-VERSION.sh`, `openagentinbox-VERSION-source.zip`, the clients
for linux-arm64, linux-amd64 and darwin-arm64, the Python wheel, `MANIFEST.json` and
`SHA256SUMS`. It prints the toolchain versions, the commit and every checksum.

The build never touches the working tree and never deletes a path it was given. It refuses a
dirty tree (there is no override) and captures the commit once; the tree, the source date and
both exports (the build tree and the source zip) derive from that commit, never from HEAD again.
It builds in a work directory whose path depends only on the commit (`/tmp/openagentinbox-release-COMMIT`, whatever `TMPDIR` is, created with `mkdir`; a leftover is refused, never reused), so a tool whose output depends on the build path gives the same bytes every time, with `npm ci` from the lockfile, `GOTOOLCHAIN=local` and
read-only Go modules, and fails if HEAD or the tree moved during the build. The output must be
a new name directly under `build/` (which must exist): the directory is claimed with one
`mkdir`, so two builds cannot share it, and three tests in `make verify-go` check it: `tests/release-outdir.sh` (the output guard),
`tests/release-package.sh` (the installer and wheel are packaged to the same bytes from the same
files, with Python's standard library only, no `tar` or `gzip` binary) and
`tests/release-source-time.sh` (the source-zip check under several time zones; the build runs
with `TZ=UTC` because `git archive` writes zip times in local time). Remove an old build yourself before reusing its name. `MANIFEST.json` records
the commit, the tree, the source date, the sha256 of the exported source tar, the toolchain
versions, the lockfile and `go.sum` hashes, each file's sha256 and the sha256 of every file inside the installer payload (so a failed `make release-check` shows which file differs in the manifest diff), with no time or host name; the
build fails unless the source zip carries the commit id, the commit time and exactly the
exported files. Every Go binary uses `-trimpath` and an empty build id, archive times come from
the commit time, and the Next build id is the commit. `make release-check` builds twice, the second time with another `TMPDIR`, and fails
unless both `SHA256SUMS` match, so a reproducibility claim needs a receipt from it. Publish the
files, `MANIFEST.json` and `SHA256SUMS` unchanged with the tag.

Build web release archives on ENG-UK. Test deployable Go services on their
target OS and architecture. Run Darwin tools on ENG-UK. The small VPSes
are for load, concurrency and soak measurements. Do not build or test
candidates on the Mac mini.

See [web verification](web/README.md#verification) for the daemon fixtures
and [stress testing](docs/stress-testing.md) for load jobs.
