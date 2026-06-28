# wasmdesk

<!-- banner placeholder: drop assets/banner.png here when the brand kit is ready -->

The **wasmdesk** meta-project brings up the whole [wasmdesk](https://github.com/wasmdesk)
family — login portal, two browser desktops, OCI registry, optional Quake asset
push — with a single command.

```sh
git clone https://github.com/wasmdesk/wasmdesk
cd wasmdesk
task up
open http://localhost:9000   # the login portal
```

`alice` / `x` (the demo credentials baked into
[wasmlogin](https://github.com/wasmdesk/wasmlogin)) gets you in; pick **wasmbox**
or **wasmaqua** from the combo box and the portal proxies you straight onto the
in-browser compositor.

## Architecture

```
                        ┌──────────────────────────────────────┐
                        │            task up (Go)              │
                        │   cmd/up = process supervisor +      │
                        │   phase-ordered health checks        │
                        └──────────────────────────────────────┘
                                          │
                  ┌───────────────────────┼───────────────────────────┐
                  │                       │                           │
                  ▼                       ▼                           ▼
        ┌──────────────────┐   ┌──────────────────┐    ┌──────────────────────┐
        │  OCI registry    │   │ build phase      │    │  Quake asset push    │
        │  registry:2 on   │   │ task -d wasmbox  │    │  task -d engine      │
        │  :5000 (Docker,  │   │   build:all      │    │   oci-pack &&        │
        │  CORS+CORP)      │   │ task -d wasmaqua │    │   oci-push           │
        │  reuse if alive  │   │   build          │    │  best-effort         │
        └──────────────────┘   │ task -d wasmlogin│    └──────────────────────┘
                               │   build          │
                               └──────────────────┘
                                          │
                                          ▼
                  ┌─────────────────────────────────────────────────┐
                  │                spawn phase                      │
                  │                                                 │
                  │  wasmbox-serve   :8080  (Ruby compositor wasm)  │
                  │  wasmaqua-serve  :8081  (Aqua-themed sibling)   │
                  │  wasmlogin       :9000  (portal + reverse-proxy)│
                  │                                                 │
                  │  stdout/stderr is tailed with coloured prefixes │
                  │  [wasmbox]  [wasmaqua]  [wasmlogin]             │
                  └─────────────────────────────────────────────────┘
                                          │
                                          ▼
                       ┌──────────────────────────────────┐
                       │  health-poll all four endpoints  │
                       │  until 200 OK or 10 s timeout    │
                       │  → "wasmdesk stack up" banner    │
                       └──────────────────────────────────┘
```

## Phases

`cmd/up` runs strictly in order. Each phase fails loud; later phases never start
if an earlier one did not finish.

1. **registry** — probe `:5000`; if `/v2/` already serves, reuse. Otherwise
   `docker run registry:2` with CORS/CORP headers (`Access-Control-Allow-Origin: ["*"]`,
   the quoted form that does not get parsed as a YAML anchor).
2. **build** — invoke each sibling repo's own `task build` (or `build:all`);
   skip with `SKIP_BUILD=1`.
3. **quake push** — `task -d $ENGINE_DIR oci-pack && task -d $ENGINE_DIR oci-push`;
   best-effort, skipped automatically if no `pak0.pak` is present or
   `SKIP_QUAKE_PUSH=1`.
4. **spawn** — start the three serve binaries and tail their output.
5. **health** — poll every 200 ms until all endpoints respond 200 OK, then print
   the banner.

`SIGINT` / `SIGTERM` runs the supervisor's `StopAll`: every child gets `SIGTERM`,
3 s grace, then `SIGKILL` to stragglers.

## Configuration

All knobs are env vars; the sibling repo paths default to `../<name>` (the
[wasmdesk layout convention](https://github.com/wasmdesk)):

| variable             | default                              |
| -------------------- | ------------------------------------ |
| `WASMBOX_DIR`        | `../wasmbox`                         |
| `WASMAQUA_DIR`       | `../wasmaqua`                        |
| `WASMLOGIN_DIR`      | `../wasmlogin`                       |
| `ENGINE_DIR`         | `../../go-quake1/engine`             |
| `OCIAPPS_DIR`        | `../ociapps`                         |
| `WASMBOX_PORT`       | `8080`                               |
| `WASMAQUA_PORT`      | `8081`                               |
| `WASMLOGIN_PORT`     | `9000`                               |
| `REGISTRY_PORT`      | `5000`                               |
| `SKIP_BUILD`         | `0`                                  |
| `SKIP_REGISTRY`      | `0`                                  |
| `SKIP_QUAKE_PUSH`    | `0`                                  |

## Tasks

```
task up        # the whole stack
task down      # SIGTERM the serve binaries + stop the registry container
task build     # build only — does not start anything
task status    # GET /healthz on each endpoint
task logs      # tail the supervisor log file
```

## License

BSD-3-Clause; see [LICENSE](LICENSE). Copyright the wasmdesk/wasmdesk authors.
