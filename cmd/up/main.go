// Copyright (c) 2026, the wasmdesk/wasmdesk authors. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

// Command up is the wasmdesk meta-project orchestrator.
//
// It runs the stack bring-up in six strictly ordered phases:
//
//  1. registry  -- probe :5000, reuse if a registry is already serving /v2/,
//     otherwise `docker run registry:2` with CORS+CORP headers wide open.
//  2. refresh   -- (a) before build: delete sibling wasm artifacts so Taskfile
//     `generates:` rules see them as missing and rebuild from the latest
//     cross-repo sources; (b) after build: re-pack + re-push the OCI images
//     (terminal/files/hello/dock/code + Quake assets) so the bytes served from
//     the registry match the freshly built wasms. Gate with SKIP_REFRESH=1
//     when iterating fast and you trust the on-disk state.
//  3. build     -- delegate to each sibling repo's own `task build` target.
//  4. quake     -- best-effort `task -d $ENGINE_DIR oci-pack && oci-push`.
//  5. spawn     -- launch wasmbox-serve / wasmaqua-serve / wasmlogin and tail
//     their stdout/stderr with coloured "[name] " prefixes.
//  6. health    -- poll /healthz (or /) on each endpoint every 200 ms until all
//     respond 200 or 10 s elapses; print the "stack up" banner on success.
//
// On SIGINT/SIGTERM the supervisor's StopAll runs: every child gets SIGTERM,
// then a 3 s grace period, then SIGKILL for stragglers; the registry container
// is left running because it is a shared resource (`task down` removes it).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Config bundles every knob `task up` reads from env + flags. Defaults match
// the wasmdesk layout convention: sibling repos at `../<name>`, the engine at
// `../../go-quake1/engine`. Each phase reads from one Config; tests build one
// inline.
type Config struct {
	WasmboxDir   string
	WasmaquaDir  string
	WasmloginDir string
	EngineDir    string
	OciappsDir   string

	WasmboxPort   int
	WasmaquaPort  int
	WasmloginPort int
	RegistryPort  int

	SkipBuild     bool
	SkipRegistry  bool
	SkipQuakePush bool
	// SkipRefresh disables the cross-repo wasm/OCI refresh phase. Default is
	// false (refresh runs) so every `task up` picks up sibling-repo source
	// changes. Set to true for fast iteration via `task up:fast`.
	SkipRefresh bool

	// HealthTimeout caps how long Phase 5 waits before declaring failure.
	HealthTimeout time.Duration
	// StopGrace is how long children get to exit after SIGTERM before we
	// SIGKILL them.
	StopGrace time.Duration

	// Stdout/Stderr are the writers the orchestrator's own logs + the
	// children's prefixed streams go to. Tests inject bytes.Buffer.
	Stdout io.Writer
	Stderr io.Writer

	// Mode is `up` (default) | `down` | `status`. Driven by -mode flag.
	Mode string
}

// LoadConfig parses flags + env into a Config. `getenv` is injected so tests
// can drive every code path without touching the process environment.
func LoadConfig(args []string, getenv func(string) string, stdout, stderr io.Writer) (Config, error) {
	fs := flag.NewFlagSet("wasmdesk-up", flag.ContinueOnError)
	fs.SetOutput(stderr)

	wasmboxDir := fs.String("wasmbox-dir", envOr(getenv, "WASMBOX_DIR", "../wasmbox"), "wasmbox repo path")
	wasmaquaDir := fs.String("wasmaqua-dir", envOr(getenv, "WASMAQUA_DIR", "../wasmaqua"), "wasmaqua repo path")
	wasmloginDir := fs.String("wasmlogin-dir", envOr(getenv, "WASMLOGIN_DIR", "../wasmlogin"), "wasmlogin repo path")
	engineDir := fs.String("engine-dir", envOr(getenv, "ENGINE_DIR", "../../go-quake1/engine"), "go-quake1/engine repo path")
	ociappsDir := fs.String("ociapps-dir", envOr(getenv, "OCIAPPS_DIR", "../ociapps"), "ociapps repo path")

	wasmboxPort := fs.Int("wasmbox-port", envOrInt(getenv, "WASMBOX_PORT", 8080), "wasmbox port")
	wasmaquaPort := fs.Int("wasmaqua-port", envOrInt(getenv, "WASMAQUA_PORT", 8081), "wasmaqua port")
	wasmloginPort := fs.Int("wasmlogin-port", envOrInt(getenv, "WASMLOGIN_PORT", 9000), "wasmlogin port")
	registryPort := fs.Int("registry-port", envOrInt(getenv, "REGISTRY_PORT", 5000), "OCI registry port")

	skipBuild := fs.Bool("skip-build", envOrBool(getenv, "SKIP_BUILD", false), "skip the build phase")
	skipRegistry := fs.Bool("skip-registry", envOrBool(getenv, "SKIP_REGISTRY", false), "skip the registry phase")
	skipQuakePush := fs.Bool("skip-quake-push", envOrBool(getenv, "SKIP_QUAKE_PUSH", false), "skip the Quake OCI push phase")
	skipRefresh := fs.Bool("skip-refresh", envOrBool(getenv, "SKIP_REFRESH", false), "skip the cross-repo wasm/OCI refresh phase")
	mode := fs.String("mode", "up", "operation mode: up | down | status | refresh")

	healthTimeout := fs.Duration("health-timeout", envOrDuration(getenv, "HEALTH_TIMEOUT", 10*time.Second), "health-poll timeout")
	stopGrace := fs.Duration("stop-grace", envOrDuration(getenv, "STOP_GRACE", 3*time.Second), "SIGTERM grace period")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg := Config{
		WasmboxDir: *wasmboxDir, WasmaquaDir: *wasmaquaDir, WasmloginDir: *wasmloginDir,
		EngineDir: *engineDir, OciappsDir: *ociappsDir,
		WasmboxPort: *wasmboxPort, WasmaquaPort: *wasmaquaPort, WasmloginPort: *wasmloginPort,
		RegistryPort:  *registryPort,
		SkipBuild:     *skipBuild,
		SkipRegistry:  *skipRegistry,
		SkipQuakePush: *skipQuakePush,
		SkipRefresh:   *skipRefresh,
		HealthTimeout: *healthTimeout,
		StopGrace:     *stopGrace,
		Stdout:        stdout,
		Stderr:        stderr,
		Mode:          *mode,
	}
	return cfg, nil
}

// ---- orchestration ----

// Run is the orchestrator entrypoint. It is split out from main() so tests can
// drive it with custom Config + Runner. The returned error is the user-visible
// exit reason; nil means clean shutdown.
func Run(ctx context.Context, cfg Config, r Runner, signalCh <-chan os.Signal) error {
	// Serialise writes to the user-supplied writers; the supervisor + main
	// loop + test observers all touch them concurrently.
	cfg.Stdout = &syncWriter{w: cfg.Stdout}
	cfg.Stderr = &syncWriter{w: cfg.Stderr}
	switch cfg.Mode {
	case "", "up":
		return runUp(ctx, cfg, r, signalCh)
	case "down":
		return runDown(ctx, cfg, r)
	case "status":
		return runStatus(ctx, cfg)
	case "refresh":
		return runRefresh(ctx, cfg, r)
	default:
		return fmt.Errorf("unknown mode %q (want up|down|status|refresh)", cfg.Mode)
	}
}

// runUp executes the five phases in order. A failure in registry/build/spawn is
// fatal; the Quake push phase is best-effort and only logs.
func runUp(ctx context.Context, cfg Config, r Runner, signalCh <-chan os.Signal) error {
	logf(cfg.Stdout, "wasmdesk-up: starting")

	// Phase 1 -- registry.
	if !cfg.SkipRegistry {
		if err := PhaseRegistry(ctx, cfg, r); err != nil {
			return fmt.Errorf("registry phase: %w", err)
		}
	} else {
		logf(cfg.Stdout, "[skip] registry phase (SKIP_REGISTRY=1)")
	}

	// Phase 2a -- refresh (clean stale wasm artifacts so the upcoming build
	// phase rebuilds from the latest sibling-repo sources).
	if !cfg.SkipRefresh {
		if err := PhaseRefreshClean(ctx, cfg); err != nil {
			return fmt.Errorf("refresh-clean phase: %w", err)
		}
	} else {
		logf(cfg.Stdout, "[skip] refresh phase (SKIP_REFRESH=1)")
	}

	// Phase 3 -- build.
	if !cfg.SkipBuild {
		if err := PhaseBuild(ctx, cfg, r); err != nil {
			return fmt.Errorf("build phase: %w", err)
		}
	} else {
		logf(cfg.Stdout, "[skip] build phase (SKIP_BUILD=1)")
	}

	// Phase 2b -- refresh (re-pack + re-push OCI images now that the wasms
	// reflect the new sources; best-effort because it depends on a live
	// registry, but a registry failure here means the browser would serve
	// stale bytes, so we surface it).
	if !cfg.SkipRefresh {
		if err := PhaseRefreshRepack(ctx, cfg, r); err != nil {
			logf(cfg.Stderr, "refresh-repack phase (best-effort): %v", err)
		}
	}

	// Phase 4 -- Quake OCI push (best-effort).
	if !cfg.SkipQuakePush {
		if err := PhaseQuakePush(ctx, cfg, r); err != nil {
			logf(cfg.Stderr, "quake push phase (best-effort): %v", err)
		}
	} else {
		logf(cfg.Stdout, "[skip] quake-push phase (SKIP_QUAKE_PUSH=1)")
	}

	// Phase 4 -- spawn.
	sup, err := PhaseSpawn(ctx, cfg, r)
	if err != nil {
		return fmt.Errorf("spawn phase: %w", err)
	}

	// Phase 5 -- health.
	if err := PhaseHealth(ctx, cfg); err != nil {
		// On a health failure, tear down so we don't leave orphan children.
		_ = sup.StopAll(ctx, cfg.StopGrace)
		return fmt.Errorf("health phase: %w", err)
	}

	logf(cfg.Stdout, "wasmdesk stack up at http://localhost:%d (login portal)", cfg.WasmloginPort)
	logf(cfg.Stdout, "  - wasmbox    http://localhost:%d", cfg.WasmboxPort)
	logf(cfg.Stdout, "  - wasmaqua   http://localhost:%d", cfg.WasmaquaPort)
	logf(cfg.Stdout, "  - registry   http://localhost:%d/v2/", cfg.RegistryPort)
	logf(cfg.Stdout, "press Ctrl+C to stop")

	// Wait for either a signal or a child exit.
	select {
	case sig := <-signalCh:
		logf(cfg.Stdout, "received %s; stopping...", sig)
	case <-ctx.Done():
		logf(cfg.Stdout, "context cancelled; stopping...")
	case <-sup.WaitAny():
		logf(cfg.Stderr, "a child process exited unexpectedly; stopping the rest")
	}
	return sup.StopAll(ctx, cfg.StopGrace)
}

// runDown is `task down`: SIGTERM the serve binaries by name + remove the
// registry container.
func runDown(ctx context.Context, cfg Config, r Runner) error {
	logf(cfg.Stdout, "wasmdesk-up: stopping")
	// pkill is best-effort; ignore its exit code so a clean tree (no
	// matching processes) is not a failure.
	for _, name := range []string{"wasmbox-serve", "wasmaqua-serve", "wasmlogin"} {
		_ = r.Run(ctx, ".", nil, "pkill", "-TERM", "-f", name)
	}
	_ = r.Run(ctx, ".", nil, "docker", "rm", "-f", "wasmdesk-registry")
	_ = r.Run(ctx, ".", nil, "docker", "rm", "-f", "oci-registry")
	_ = r.Run(ctx, ".", nil, "docker", "rm", "-f", "ociapps-registry")
	return nil
}

// runStatus prints a one-line per-endpoint OK/DOWN summary. Useful for CI.
func runStatus(ctx context.Context, cfg Config) error {
	endpoints := map[string]string{
		"registry":  fmt.Sprintf("http://localhost:%d/v2/", cfg.RegistryPort),
		"wasmbox":   fmt.Sprintf("http://localhost:%d/", cfg.WasmboxPort),
		"wasmaqua":  fmt.Sprintf("http://localhost:%d/", cfg.WasmaquaPort),
		"wasmlogin": fmt.Sprintf("http://localhost:%d/", cfg.WasmloginPort),
	}
	var hadErr bool
	for name, url := range endpoints {
		if err := pingOnce(ctx, url); err != nil {
			logf(cfg.Stdout, "  %-10s DOWN  (%s) %v", name, url, err)
			hadErr = true
			continue
		}
		logf(cfg.Stdout, "  %-10s OK    (%s)", name, url)
	}
	if hadErr {
		return fmt.Errorf("one or more endpoints are down")
	}
	return nil
}

// ---- phases ----

// PhaseRegistry brings the OCI registry up on cfg.RegistryPort. It reuses an
// existing registry if /v2/ is already serving, else `docker run registry:2`
// with CORS+CORP headers (the [*] quoted form -- bare [*] is parsed as a YAML
// anchor and crashes the registry on boot, the same gotcha we hit in wasmbox's
// own Taskfile).
func PhaseRegistry(ctx context.Context, cfg Config, r Runner) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/", cfg.RegistryPort)
	if err := pingOnce(ctx, url); err == nil {
		logf(cfg.Stdout, "[registry] reusing existing registry on :%d", cfg.RegistryPort)
		return nil
	}
	logf(cfg.Stdout, "[registry] starting fresh registry:2 on :%d", cfg.RegistryPort)
	env := []string{
		`REGISTRY_HTTP_HEADERS_Access-Control-Allow-Origin=["*"]`,
		`REGISTRY_HTTP_HEADERS_Access-Control-Allow-Methods=["GET","HEAD","OPTIONS"]`,
		`REGISTRY_HTTP_HEADERS_Access-Control-Allow-Headers=["Authorization","Accept"]`,
		`REGISTRY_HTTP_HEADERS_Cross-Origin-Resource-Policy=["cross-origin"]`,
	}
	args := []string{"run", "-d", "--rm", "--name", "wasmdesk-registry",
		"-p", fmt.Sprintf("%d:5000", cfg.RegistryPort)}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, "registry:2")
	if err := r.Run(ctx, ".", nil, "docker", args...); err != nil {
		return fmt.Errorf("docker run registry:2: %w", err)
	}
	// Poll until ready (10 s budget).
	return WaitForHTTP(ctx, url, cfg.HealthTimeout)
}

// PhaseBuild delegates to each sibling repo's own `task build` (or build:all
// for wasmbox, which has both targets and we want the full client matrix).
func PhaseBuild(ctx context.Context, cfg Config, r Runner) error {
	type job struct {
		name, dir, target string
	}
	jobs := []job{
		{"wasmbox", cfg.WasmboxDir, "build:all"},
		{"wasmaqua", cfg.WasmaquaDir, "build"},
		{"wasmlogin", cfg.WasmloginDir, "build"},
	}
	for _, j := range jobs {
		if !dirExists(j.dir) {
			return fmt.Errorf("%s dir not found: %s", j.name, j.dir)
		}
		logf(cfg.Stdout, "[build] %s -> task %s", j.name, j.target)
		if err := r.Run(ctx, j.dir, nil, "task", j.target); err != nil {
			return fmt.Errorf("%s build: %w", j.name, err)
		}
	}
	return nil
}

// refreshClients is the canonical list of wasmbox sibling-client apps that
// PhaseRefresh wipes + repacks + repushes. Keep in sync with the
// `ociapps:pack:<app>` / `ociapps:push:<app>` targets in wasmbox/Taskfile.yml
// (quake is handled separately by PhaseQuakePush).
var refreshClients = []string{"terminal", "files", "hello", "dock", "code"}

// refreshArtifactPaths returns the absolute paths of the wasm artifacts that
// PhaseRefreshClean removes. Split out (a) so tests can assert the path set
// without re-implementing it, (b) so we have one place to maintain when
// sibling repos grow new clients.
//
// For each entry the corresponding source repo + Taskfile target is:
//   wasmbox/clients/{app}/{app}.wasm      -> wasmbox    `task build:{app}`
//   wasmbox/wasmbox.wasm                  -> wasmbox    `task build:compositor`
//   wasmbox/clients/quake/quake.wasm      -> wasmbox    `task build:quake`
//   wasmaqua/wasmaqua.wasm                -> wasmaqua   `task build:compositor`
func refreshArtifactPaths(cfg Config) []string {
	paths := []string{
		filepath.Join(cfg.WasmboxDir, "wasmbox.wasm"),
		filepath.Join(cfg.WasmboxDir, "clients", "quake", "quake.wasm"),
		filepath.Join(cfg.WasmaquaDir, "wasmaqua.wasm"),
	}
	for _, app := range refreshClients {
		paths = append(paths, filepath.Join(cfg.WasmboxDir, "clients", app, app+".wasm"))
	}
	return paths
}

// removeFn is the file-removal seam; tests override it to record removals
// without touching the real filesystem.
var removeFn = os.Remove

// PhaseRefreshClean wipes every wasm artifact PhaseBuild is responsible for
// regenerating. Removal is best-effort: a missing file (the first-ever build)
// is fine, but other errors are surfaced so the user sees genuine fs problems.
//
// Why this exists: wasmbox/Taskfile has `generates:` rules whose deps are
// scoped to the wasmbox source tree. When a sibling repo (engine, ocode,
// terminal) changes, the wasmbox build does not see those mtimes and skips the
// rebuild -- so a cross-repo change like the engine input-fix 9546687 would
// never reach the running stack. By removing the artifact first we force
// Task's generates-vs-deps check to rebuild.
func PhaseRefreshClean(ctx context.Context, cfg Config) error {
	logf(cfg.Stdout, "[refresh] cleaning stale wasm artifacts (forces rebuild from sibling-repo sources)")
	for _, p := range refreshArtifactPaths(cfg) {
		if err := removeFn(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		// ctx may be cancelled mid-loop; abort cleanly.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// PhaseRefreshRepack runs `task ociapps:pack:<app>` then `task ociapps:push:<app>`
// inside wasmbox for every entry in refreshClients. Without this the OCI
// registry keeps serving the previous wasm digests and the browser pulls
// stale code even though disk has fresh bytes. The first error stops the
// loop; runUp logs it as best-effort because a missing registry is recoverable
// (the next `task up` will retry).
func PhaseRefreshRepack(ctx context.Context, cfg Config, r Runner) error {
	if !dirExists(cfg.WasmboxDir) {
		logf(cfg.Stdout, "[refresh] wasmbox dir not found at %s, skipping OCI repack", cfg.WasmboxDir)
		return nil
	}
	for _, app := range refreshClients {
		logf(cfg.Stdout, "[refresh] re-pack + re-push OCI image: %s", app)
		if err := r.Run(ctx, cfg.WasmboxDir, nil, "task", "ociapps:pack:"+app); err != nil {
			return fmt.Errorf("ociapps:pack:%s: %w", app, err)
		}
		if err := r.Run(ctx, cfg.WasmboxDir, nil, "task", "ociapps:push:"+app); err != nil {
			return fmt.Errorf("ociapps:push:%s: %w", app, err)
		}
	}
	return nil
}

// runRefresh is the entry point for `task refresh`: do a full clean +
// rebuild + repack + repush of the cross-repo wasm + OCI assets without
// spawning the serve binaries. Useful for "I changed a sibling, push the new
// bytes to my running stack". Registry/quake-push are reused from the up
// path so behaviour matches `task up`.
func runRefresh(ctx context.Context, cfg Config, r Runner) error {
	logf(cfg.Stdout, "wasmdesk-up: refresh (clean + rebuild + repack + repush)")
	if !cfg.SkipRegistry {
		if err := PhaseRegistry(ctx, cfg, r); err != nil {
			return fmt.Errorf("registry phase: %w", err)
		}
	}
	if err := PhaseRefreshClean(ctx, cfg); err != nil {
		return fmt.Errorf("refresh-clean phase: %w", err)
	}
	if !cfg.SkipBuild {
		if err := PhaseBuild(ctx, cfg, r); err != nil {
			return fmt.Errorf("build phase: %w", err)
		}
	}
	if err := PhaseRefreshRepack(ctx, cfg, r); err != nil {
		return fmt.Errorf("refresh-repack phase: %w", err)
	}
	if !cfg.SkipQuakePush {
		if err := PhaseQuakePush(ctx, cfg, r); err != nil {
			logf(cfg.Stderr, "quake push phase (best-effort): %v", err)
		}
	}
	logf(cfg.Stdout, "wasmdesk-up: refresh done")
	return nil
}

// PhaseQuakePush runs the engine's oci-pack + oci-push. Best-effort: we skip
// silently when the engine dir is missing or there is no pak0 next to it.
func PhaseQuakePush(ctx context.Context, cfg Config, r Runner) error {
	if !dirExists(cfg.EngineDir) {
		logf(cfg.Stdout, "[quake-push] engine dir not found at %s, skipping", cfg.EngineDir)
		return nil
	}
	// No pak0 -> oci-pack will fail; skip with a friendly message instead of
	// burning user time on a known no-op.
	pak := filepath.Join(cfg.EngineDir, "id1", "pak0.pak")
	if !fileExists(pak) {
		logf(cfg.Stdout, "[quake-push] no pak0 at %s, skipping (provide it to enable Quake)", pak)
		return nil
	}
	logf(cfg.Stdout, "[quake-push] packing Quake assets")
	if err := r.Run(ctx, cfg.EngineDir, nil, "task", "oci-pack"); err != nil {
		return fmt.Errorf("oci-pack: %w", err)
	}
	logf(cfg.Stdout, "[quake-push] pushing Quake assets")
	if err := r.Run(ctx, cfg.EngineDir, nil, "task", "oci-push"); err != nil {
		return fmt.Errorf("oci-push: %w", err)
	}
	return nil
}

// PhaseSpawn starts the three serve binaries with prefix-tagged logging, then
// returns the Supervisor so the caller can manage lifecycle.
func PhaseSpawn(ctx context.Context, cfg Config, r Runner) (*Supervisor, error) {
	sup := NewSupervisor(cfg.Stdout, cfg.Stderr)

	type spec struct {
		name, dir, bin, color string
		args, env             []string
	}
	specs := []spec{
		{
			name: "wasmbox", dir: cfg.WasmboxDir,
			bin:   filepath.Join(cfg.WasmboxDir, "bin", "wasmbox-serve"),
			args:  []string{"-addr", fmt.Sprintf(":%d", cfg.WasmboxPort)},
			color: "\033[36m", // cyan
		},
		{
			name: "wasmaqua", dir: cfg.WasmaquaDir,
			bin:   filepath.Join(cfg.WasmaquaDir, "bin", "wasmaqua-serve"),
			args:  []string{"-addr", fmt.Sprintf(":%d", cfg.WasmaquaPort)},
			color: "\033[35m", // magenta
		},
		{
			name: "wasmlogin", dir: cfg.WasmloginDir,
			bin: filepath.Join(cfg.WasmloginDir, "bin", "wasmlogin"),
			args: []string{
				"-addr", fmt.Sprintf(":%d", cfg.WasmloginPort),
				"-wasmbox-url", fmt.Sprintf("http://localhost:%d", cfg.WasmboxPort),
				"-wasmaqua-url", fmt.Sprintf("http://localhost:%d", cfg.WasmaquaPort),
			},
			color: "\033[33m", // yellow
		},
	}
	for _, s := range specs {
		if err := sup.Start(ctx, r, s.name, s.color, s.dir, s.bin, s.args, s.env); err != nil {
			_ = sup.StopAll(ctx, cfg.StopGrace)
			return nil, fmt.Errorf("start %s: %w", s.name, err)
		}
	}
	return sup, nil
}

// PhaseHealth polls all four endpoints concurrently in 200 ms intervals until
// all return 200, or the timeout elapses.
func PhaseHealth(ctx context.Context, cfg Config) error {
	urls := []string{
		fmt.Sprintf("http://127.0.0.1:%d/v2/", cfg.RegistryPort),
		fmt.Sprintf("http://127.0.0.1:%d/", cfg.WasmboxPort),
		fmt.Sprintf("http://127.0.0.1:%d/", cfg.WasmaquaPort),
		fmt.Sprintf("http://127.0.0.1:%d/", cfg.WasmloginPort),
	}
	return WaitForAll(ctx, urls, cfg.HealthTimeout)
}

// ---- helpers ----

// envOr returns getenv(k) if set, else def.
func envOr(getenv func(string) string, k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}

func envOrInt(getenv func(string) string, k string, def int) int {
	v := getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func envOrBool(getenv func(string) string, k string, def bool) bool {
	v := getenv(k)
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	case "0", "false", "FALSE", "no", "off":
		return false
	}
	return def
}

func envOrDuration(getenv func(string) string, k string, def time.Duration) time.Duration {
	v := getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// dirExists / fileExists are split for clarity in the (rare) call sites.
var statFn = os.Stat // overridable for tests

func dirExists(p string) bool {
	fi, err := statFn(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := statFn(p)
	return err == nil && !fi.IsDir()
}

// pingOnce GETs url with a short timeout and returns nil if the status is 2xx.
// httpClient is overridable so tests can inject a httptest.NewServer or a
// custom Transport.
var httpClient = &http.Client{Timeout: 2 * time.Second}

func pingOnce(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}

// logf is the orchestrator's own logging seam. It writes to cfg.Stdout (which
// tests replace with a buffer) and prefixes every line with [up]. Always ends
// with a newline so cmd/up's stdout looks like a log even though we are not
// using log.Logger.
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "[up] "+format+"\n", args...)
}

// main is a 3-line shim so the real logic lives in Run() and stays testable.
// osExit is a var so tests can override it.
var osExit = os.Exit

func main() {
	stdout, stderr := os.Stdout, os.Stderr
	cfg, err := LoadConfig(os.Args[1:], os.Getenv, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		osExit(2)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	if err := Run(ctx, cfg, NewExecRunner(stdout, stderr), sigCh); err != nil {
		fmt.Fprintln(stderr, err)
		osExit(1)
		return
	}
}
