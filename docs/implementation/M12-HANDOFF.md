# M12 WebUI handoff

## Distribution and startup

The optional UI is served publicly beneath `/admin/`; the authenticated API
remains `/admin/v1` on the separate admin listener. Go-only builds embed the
checked-in placeholder and return HTTP 503 from `/admin/` when a frontend bundle
has not been packaged. They do not need Node or generated frontend files:

```sh
go build ./cmd/gateway
go test ./...
```

The default release script remains Go-only. UI packaging is an explicit option
and is the only release path requiring Node: use Node.js `22.16.0` and npm
`10.9.2`, then run from the repository root:

```sh
./scripts/release-build.sh --with-ui bin/gateway
npm --prefix web run e2e:install
M12_E2E_GATEWAY="$PWD/bin/gateway" npm --prefix web run e2e
```

The script installs the locked frontend dependencies, builds Vite assets with
the `/admin/` base path, excludes the fallback placeholder from the packaged
bundle, temporarily embeds the built assets for a reproducible Go release build,
and restores the original assets afterward. It does not launch Node at runtime.
The Playwright dependency is pinned in `web/package-lock.json`. To validate the
local operator/gateway API separately against temporary SQLite and a deterministic
loopback fake upstream:

```sh
TMPDIR=/tmp/m12-6/t GOFLAGS=-p=1 python3 scripts/e2e-m12-webui.py
```

The operator bearer token and issued virtual key are kept in process memory by
the script and are not printed or persisted. Its test data and database are
removed on exit.

## Verified scope and limitations

Automated frontend tests cover live token memory/logout, key issuance dialog
accessibility (focus containment and Escape), one-time secret display, and
unambiguous policy labels. The packaged Chromium E2E opens a deep link under
`/admin/`, reloads it, and drives the React UI through login, account/credential
and policy/key creation, inference through the live client listener, usage
ledger, revocation, rejected inference, logout, and browser-storage checks. It
uses a real compiled gateway, SQLite, and a synthetic loopback upstream, not a
real provider. The independent gateway API script also verifies policy denial,
write-only credential responses, ledger reflection, and revocation. The package
smoke checks HTML/JS/CSS MIME, SPA fallback, missing asset 404, and API/inference
listener isolation.

The browser run used the pinned Playwright `1.58.2` package and its pinned
Chromium `145.0.7632.6` headless shell. No live external provider verification
is claimed; the synthetic upstream is not provider compatibility evidence. See
the M12-006 task card for exact Go sweep outcomes.
