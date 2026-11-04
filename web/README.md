# PestiRoute WebUI

The operator console defaults to the local Admin API at `/admin/v1`. Operator
bearer tokens remain in memory; reloading or logging out clears the session.
Demo data is available only through the explicit demo action.

Use Node.js 24 and its bundled npm. From the repository root:

```sh
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run typecheck:e2e
npm --prefix web test
npm --prefix web run build
```

For embedded release builds and real gateway/SQLite browser verification, see
[the handoff](../docs/implementation/M12-HANDOFF.md). No Node process or external
provider access is needed at gateway runtime.
