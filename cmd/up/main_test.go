// Copyright (c) 2026, the wasmdesk/wasmdesk authors. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ---- Config / env parsing ----

func emptyGetenv(string) string { return "" }

func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig_Defaults(t *testing.T) {
	var out, errb bytes.Buffer
	cfg, err := LoadConfig(nil, emptyGetenv, &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WasmboxPort != 8080 || cfg.WasmaquaPort != 8081 || cfg.WasmloginPort != 9000 || cfg.RegistryPort != 5000 {
		t.Fatalf("default ports: %+v", cfg)
	}
	if cfg.WasmboxDir != "../wasmbox" || cfg.WasmaquaDir != "../wasmaqua" || cfg.WasmloginDir != "../wasmlogin" {
		t.Fatalf("default dirs: %+v", cfg)
	}
	if cfg.EngineDir != "../../go-quake1/engine" || cfg.OciappsDir != "../ociapps" {
		t.Fatalf("default ext dirs: %+v", cfg)
	}
	if cfg.SkipBuild || cfg.SkipRegistry || cfg.SkipQuakePush {
		t.Fatalf("skip defaults: %+v", cfg)
	}
	if cfg.HealthTimeout != 10*time.Second || cfg.StopGrace != 3*time.Second {
		t.Fatalf("durations: %+v", cfg)
	}
	if cfg.Mode != "up" {
		t.Fatalf("mode: %q", cfg.Mode)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	env := map[string]string{
		"WASMBOX_DIR": "/a", "WASMAQUA_DIR": "/b", "WASMLOGIN_DIR": "/c",
		"ENGINE_DIR": "/d", "OCIAPPS_DIR": "/e",
		"WASMBOX_PORT": "1000", "WASMAQUA_PORT": "1001",
		"WASMLOGIN_PORT": "1002", "REGISTRY_PORT": "1003",
		"SKIP_BUILD": "1", "SKIP_REGISTRY": "true", "SKIP_QUAKE_PUSH": "yes",
		"HEALTH_TIMEOUT": "5s", "STOP_GRACE": "1s",
	}
	cfg, err := LoadConfig(nil, mapGetenv(env), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WasmboxDir != "/a" || cfg.WasmboxPort != 1000 || !cfg.SkipBuild ||
		!cfg.SkipRegistry || !cfg.SkipQuakePush || cfg.HealthTimeout != 5*time.Second {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
}

func TestLoadConfig_FlagBeatsEnv(t *testing.T) {
	cfg, err := LoadConfig([]string{"-wasmbox-port=2222"}, mapGetenv(map[string]string{"WASMBOX_PORT": "1234"}),
		&bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WasmboxPort != 2222 {
		t.Fatalf("flag should win, got %d", cfg.WasmboxPort)
	}
}

func TestLoadConfig_BadFlag(t *testing.T) {
	_, err := LoadConfig([]string{"-not-a-flag"}, emptyGetenv, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEnvHelpers_FallbackOnGarbage(t *testing.T) {
	// invalid integers, invalid bool keyword, invalid duration -> default returned
	if envOrInt(func(string) string { return "not-an-int" }, "X", 7) != 7 {
		t.Fatal("envOrInt should fallback")
	}
	if envOrBool(func(string) string { return "maybe" }, "X", true) != true {
		t.Fatal("envOrBool should fallback")
	}
	if envOrDuration(func(string) string { return "abc" }, "X", time.Second) != time.Second {
		t.Fatal("envOrDuration should fallback")
	}
	// explicit "false" path
	if envOrBool(func(string) string { return "false" }, "X", true) {
		t.Fatal("'false' should be false")
	}
	// envOr empty -> default
	if envOr(emptyGetenv, "X", "def") != "def" {
		t.Fatal("envOr empty -> default")
	}
	// envOr set -> value
	if envOr(func(string) string { return "v" }, "X", "def") != "v" {
		t.Fatal("envOr set -> value")
	}
}

// ---- helpers: dirExists / fileExists ----

func TestExistsHelpers(t *testing.T) {
	td := t.TempDir()
	f := filepath.Join(td, "x")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !dirExists(td) || dirExists(f) {
		t.Fatal("dirExists wrong")
	}
	if !fileExists(f) || fileExists(td) {
		t.Fatal("fileExists wrong")
	}
	if dirExists("/totally-not-here-xyz") || fileExists("/totally-not-here-xyz") {
		t.Fatal("missing path should be false")
	}
}

// ---- ping / WaitForHTTP / WaitForAll ----

func TestPingOnce_BadURL(t *testing.T) {
	// invalid url -> NewRequestWithContext fails
	if err := pingOnce(context.Background(), "://bad"); err == nil {
		t.Fatal("expected err")
	}
	// unreachable -> Do() error
	if err := pingOnce(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("expected err")
	}
}

func TestPingOnce_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	if err := pingOnce(context.Background(), srv.URL); err == nil {
		t.Fatal("expected err for 500")
	}
}

func TestWaitForHTTP_SuccessAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := WaitForHTTP(context.Background(), srv.URL, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// Timeout path: unreachable url + tiny budget.
	if err := WaitForHTTP(context.Background(), "http://127.0.0.1:1/", 50*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestWaitForHTTP_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitForHTTP(ctx, "http://127.0.0.1:1/", 5*time.Second); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestWaitForAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := WaitForAll(context.Background(), nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitForAll(context.Background(), []string{srv.URL, srv.URL}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitForAll(context.Background(), []string{srv.URL, "http://127.0.0.1:1/"}, 100*time.Millisecond); err == nil {
		t.Fatal("expected err")
	}
}

// ---- fake Runner ----

type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	runErr  map[string]error
	spawnEr map[string]error
	procs   []*fakeProc
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runErr: map[string]error{}, spawnEr: map[string]error{}}
}

func (r *fakeRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, "RUN "+dir+" "+key)
	return r.runErr[name]
}

func (r *fakeRunner) Spawn(ctx context.Context, dir string, env []string, name string, args ...string) (Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "SPAWN "+dir+" "+name)
	if err := r.spawnEr[name]; err != nil {
		return nil, err
	}
	p := newFakeProc(name)
	r.procs = append(r.procs, p)
	return p, nil
}

type fakeProc struct {
	name     string
	stdoutR  *io.PipeReader
	stdoutW  *io.PipeWriter
	stderrR  *io.PipeReader
	stderrW  *io.PipeWriter
	stopCh   chan struct{}
	stopOnce sync.Once
	signaled atomic.Int32
}

func newFakeProc(name string) *fakeProc {
	soR, soW := io.Pipe()
	seR, seW := io.Pipe()
	p := &fakeProc{name: name, stdoutR: soR, stdoutW: soW, stderrR: seR, stderrW: seW, stopCh: make(chan struct{})}
	// Emit one line so the tail goroutine sees something.
	go func() {
		_, _ = io.WriteString(soW, "hello from "+name+"\n")
	}()
	return p
}

func (p *fakeProc) Stdout() io.Reader { return p.stdoutR }
func (p *fakeProc) Stderr() io.Reader { return p.stderrR }
func (p *fakeProc) Signal(sig os.Signal) error {
	p.signaled.Add(1)
	p.stopOnce.Do(func() {
		close(p.stopCh)
		_ = p.stdoutW.Close()
		_ = p.stderrW.Close()
	})
	return nil
}
func (p *fakeProc) Wait() error {
	<-p.stopCh
	return nil
}

// ---- supervisor ----

func TestSupervisor_StartStop(t *testing.T) {
	var out, errb safeBuf
	sup := NewSupervisor(&out, &errb)
	r := newFakeRunner()
	ctx := context.Background()
	if err := sup.Start(ctx, r, "alpha", "\033[36m", ".", "/bin/alpha", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(ctx, r, "beta", "\033[35m", ".", "/bin/beta", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Give the tail goroutines a tick to flush.
	time.Sleep(50 * time.Millisecond)
	if err := sup.StopAll(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	// WaitAny should be already closed.
	select {
	case <-sup.WaitAny():
	case <-time.After(time.Second):
		t.Fatal("WaitAny never fired")
	}
	if !strings.Contains(out.String(), "alpha") {
		t.Fatalf("expected prefixed alpha output, got: %q", out.String())
	}
}

func TestSupervisor_StopAll_KillsStragglers(t *testing.T) {
	// A child whose Signal does not actually stop it; StopAll should send
	// SIGKILL after grace expires and return cleanly.
	var out, errb bytes.Buffer
	sup := NewSupervisor(&out, &errb)
	stub := &stubProc{}
	c := &child{name: "stub", proc: stub, doneCh: make(chan error, 1)}
	sup.children = append(sup.children, c)
	if err := sup.StopAll(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if stub.kills.Load() == 0 {
		t.Fatal("expected SIGKILL after grace")
	}
}

type stubProc struct {
	terms, kills atomic.Int32
}

func (s *stubProc) Stdout() io.Reader { return strings.NewReader("") }
func (s *stubProc) Stderr() io.Reader { return strings.NewReader("") }
func (s *stubProc) Signal(sig os.Signal) error {
	if sig == syscall.SIGTERM {
		s.terms.Add(1)
	}
	if sig == syscall.SIGKILL {
		s.kills.Add(1)
	}
	return nil
}
func (s *stubProc) Wait() error { return nil }

func TestSupervisor_TailNilReader(t *testing.T) {
	sup := NewSupervisor(io.Discard, io.Discard)
	sup.tail(&child{name: "x"}, nil, false) // must return cleanly
}

func TestFirstDone_AlreadyClosed(t *testing.T) {
	c := &child{doneCh: make(chan error, 1)}
	c.doneCh <- nil
	ch := firstDone([]*child{c})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("firstDone with closed channel never fired")
	}
}

func TestFirstDone_FansIn(t *testing.T) {
	c1 := &child{doneCh: make(chan error, 1)}
	c2 := &child{doneCh: make(chan error, 1)}
	ch := firstDone([]*child{c1, c2})
	// Neither ready yet -> goroutine fan-in.
	time.Sleep(10 * time.Millisecond)
	c2.doneCh <- nil
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("firstDone never fired after fan-in")
	}
}

// ---- execRunner (light contract test) ----

func TestExecRunner_RunSuccess(t *testing.T) {
	var out, errb bytes.Buffer
	r := NewExecRunner(&out, &errb)
	if err := r.Run(context.Background(), ".", []string{"FOO=bar"}, "true"); err != nil {
		t.Fatal(err)
	}
}

func TestExecRunner_RunFail(t *testing.T) {
	r := NewExecRunner(io.Discard, io.Discard)
	if err := r.Run(context.Background(), ".", nil, "false"); err == nil {
		t.Fatal("expected non-zero")
	}
}

func TestExecRunner_SpawnAndWait(t *testing.T) {
	r := NewExecRunner(io.Discard, io.Discard)
	p, err := r.Spawn(context.Background(), ".", nil, "sh", "-c", "echo hi; echo err 1>&2")
	if err != nil {
		t.Fatal(err)
	}
	// Drain stdout/stderr so the child's pipe writers can close without
	// blocking on a full buffer.
	go io.Copy(io.Discard, p.Stdout())
	go io.Copy(io.Discard, p.Stderr())
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	// Signal after wait -> we test the "no process / already exited" branch
	// by signalling again; it should not panic.
	_ = p.Signal(syscall.SIGTERM)
}

func TestExecRunner_SpawnBadCommand(t *testing.T) {
	r := NewExecRunner(io.Discard, io.Discard)
	if _, err := r.Spawn(context.Background(), ".", nil, "/no/such/binary/anywhere"); err == nil {
		t.Fatal("expected err")
	}
}

func TestExecRunner_Spawn_StdoutPipeFails(t *testing.T) {
	save := stdoutPipeFn
	defer func() { stdoutPipeFn = save }()
	stdoutPipeFn = func(*exec.Cmd) (io.ReadCloser, error) { return nil, errors.New("stdout-boom") }
	r := NewExecRunner(io.Discard, io.Discard)
	if _, err := r.Spawn(context.Background(), ".", nil, "true"); err == nil {
		t.Fatal("expected stdout-pipe err")
	}
}

func TestExecRunner_Spawn_StderrPipeFails(t *testing.T) {
	save := stderrPipeFn
	defer func() { stderrPipeFn = save }()
	stderrPipeFn = func(*exec.Cmd) (io.ReadCloser, error) { return nil, errors.New("stderr-boom") }
	r := NewExecRunner(io.Discard, io.Discard)
	if _, err := r.Spawn(context.Background(), ".", nil, "true"); err == nil {
		t.Fatal("expected stderr-pipe err")
	}
}

func TestExecProc_SignalBeforeStart(t *testing.T) {
	p := &execProc{cmd: exec.Command("true")}
	if err := p.Signal(syscall.SIGTERM); err == nil {
		t.Fatal("expected error for no process")
	}
}

func TestMergeEnv(t *testing.T) {
	if got := mergeEnv([]string{"A=1"}, nil); len(got) != 1 {
		t.Fatal("empty extra should keep base")
	}
	got := mergeEnv([]string{"A=1"}, []string{"B=2"})
	if len(got) != 2 || got[1] != "B=2" {
		t.Fatalf("merged: %v", got)
	}
}

// ---- phases ----

func mustDir(t *testing.T, base, name string) string {
	t.Helper()
	d := filepath.Join(base, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// baseCfg builds a Config pointing at temp sibling dirs. w should be a *safeBuf
// (or any io.Writer) -- the production Run() wraps it in a syncWriter anyway,
// but tests that poll the buffer concurrently must use safeBuf.
func baseCfg(t *testing.T, w io.Writer) Config {
	t.Helper()
	td := t.TempDir()
	return Config{
		WasmboxDir:    mustDir(t, td, "wasmbox"),
		WasmaquaDir:   mustDir(t, td, "wasmaqua"),
		WasmloginDir:  mustDir(t, td, "wasmlogin"),
		EngineDir:     mustDir(t, td, "engine"),
		OciappsDir:    mustDir(t, td, "ociapps"),
		WasmboxPort:   18080,
		WasmaquaPort:  18081,
		WasmloginPort: 19000,
		RegistryPort:  15000,
		HealthTimeout: 200 * time.Millisecond,
		StopGrace:     50 * time.Millisecond,
		Stdout:        w,
		Stderr:        w,
		Mode:          "up",
	}
}

func TestPhaseRegistry_Reuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	port := portOf(srv.URL)
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort = port
	r := newFakeRunner()
	if err := PhaseRegistry(context.Background(), cfg, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "reusing existing") {
		t.Fatalf("expected reuse msg, got %q", out.String())
	}
}

func TestPhaseRegistry_StartFreshFails(t *testing.T) {
	// Unreachable port + fake runner whose docker run succeeds, but the
	// follow-up WaitForHTTP will time out -> PhaseRegistry returns err.
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort = 65530 // unlikely to be live
	r := newFakeRunner()
	if err := PhaseRegistry(context.Background(), cfg, r); err == nil {
		t.Fatal("expected timeout err")
	}
	// Now make docker run itself fail.
	r2 := newFakeRunner()
	r2.runErr["docker"] = errors.New("boom")
	if err := PhaseRegistry(context.Background(), cfg, r2); err == nil {
		t.Fatal("expected docker err")
	}
}

func TestPhaseBuild(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	r := newFakeRunner()
	if err := PhaseBuild(context.Background(), cfg, r); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(r.calls), r.calls)
	}
	// Missing dir -> err
	cfg.WasmboxDir = "/no/such/dir"
	if err := PhaseBuild(context.Background(), cfg, newFakeRunner()); err == nil {
		t.Fatal("expected missing-dir err")
	}
	// task fail -> err
	cfg2 := baseCfg(t, &out)
	rf := newFakeRunner()
	rf.runErr["task"] = errors.New("task fail")
	if err := PhaseBuild(context.Background(), cfg2, rf); err == nil {
		t.Fatal("expected task fail err")
	}
}

func TestPhaseQuakePush(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	// No pak0 -> skip cleanly
	if err := PhaseQuakePush(context.Background(), cfg, newFakeRunner()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no pak0") {
		t.Fatalf("expected skip msg, got %q", out.String())
	}
	// Missing engine -> skip cleanly
	cfg2 := baseCfg(t, &out)
	cfg2.EngineDir = "/no/such/engine"
	if err := PhaseQuakePush(context.Background(), cfg2, newFakeRunner()); err != nil {
		t.Fatal(err)
	}
	// With pak0 present -> runs both tasks
	pak := filepath.Join(cfg.EngineDir, "id1", "pak0.pak")
	if err := os.MkdirAll(filepath.Dir(pak), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pak, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newFakeRunner()
	if err := PhaseQuakePush(context.Background(), cfg, r); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 task calls, got %v", r.calls)
	}
	// oci-pack fail
	rf := newFakeRunner()
	rf.runErr["task"] = errors.New("pack fail")
	if err := PhaseQuakePush(context.Background(), cfg, rf); err == nil {
		t.Fatal("expected pack err")
	}
	// oci-push fail (pack ok, push fail) -- simulate by failing on the 2nd
	// task call. Our fake keys by name; both calls are "task" so we use a
	// counting wrapper.
	rc := &countingRunner{fail: 2}
	if err := PhaseQuakePush(context.Background(), cfg, rc); err == nil {
		t.Fatal("expected push err")
	}
}

// countingRunner fails on its N-th Run call.
type countingRunner struct {
	fakeRunner
	n, fail int
}

func (c *countingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	c.n++
	if c.n == c.fail {
		return errors.New("counted-fail")
	}
	return nil
}

func TestPhaseSpawn_AllOK(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	r := newFakeRunner()
	sup, err := PhaseSpawn(context.Background(), cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.StopAll(context.Background(), cfg.StopGrace); err != nil {
		t.Fatal(err)
	}
}

func TestPhaseSpawn_FirstFails(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	r := newFakeRunner()
	bin := filepath.Join(cfg.WasmboxDir, "bin", "wasmbox-serve")
	r.spawnEr[bin] = errors.New("spawn boom")
	if _, err := PhaseSpawn(context.Background(), cfg, r); err == nil {
		t.Fatal("expected err")
	}
}

func TestPhaseHealth_AllUp(t *testing.T) {
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
		}))
		return srv, portOf(srv.URL)
	}
	s1, p1 := mk()
	s2, p2 := mk()
	s3, p3 := mk()
	s4, p4 := mk()
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	defer s4.Close()
	cfg := Config{RegistryPort: p1, WasmboxPort: p2, WasmaquaPort: p3, WasmloginPort: p4, HealthTimeout: time.Second}
	if err := PhaseHealth(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestPhaseHealth_Down(t *testing.T) {
	cfg := Config{RegistryPort: 65531, WasmboxPort: 65532, WasmaquaPort: 65533, WasmloginPort: 65534, HealthTimeout: 100 * time.Millisecond}
	if err := PhaseHealth(context.Background(), cfg); err == nil {
		t.Fatal("expected err")
	}
}

// ---- Run() orchestration ----

func TestRun_UpHappy(t *testing.T) {
	// Build a config where the registry phase reuses an existing httptest,
	// the build phase runs against the fake runner, the Quake phase skips
	// (no pak), and spawn uses the fake proc. Health probes hit four
	// httptests.
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
		}))
		return srv, portOf(srv.URL)
	}
	sReg, pReg := mk()
	sBox, pBox := mk()
	sAqua, pAqua := mk()
	sLogin, pLogin := mk()
	defer sReg.Close()
	defer sBox.Close()
	defer sAqua.Close()
	defer sLogin.Close()

	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort, cfg.WasmboxPort, cfg.WasmaquaPort, cfg.WasmloginPort = pReg, pBox, pAqua, pLogin
	cfg.HealthTimeout = 500 * time.Millisecond
	cfg.StopGrace = 100 * time.Millisecond

	r := newFakeRunner()
	sigCh := make(chan os.Signal, 1)
	// Send SIGTERM after the banner.
	go func() {
		// Wait until "stack up" appears.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(out.String(), "wasmdesk stack up") {
				sigCh <- syscall.SIGTERM
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Failsafe.
		sigCh <- syscall.SIGTERM
	}()
	if err := Run(context.Background(), cfg, r, sigCh); err != nil {
		t.Fatalf("Run err: %v\n--out--\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "wasmdesk stack up") {
		t.Fatalf("missing banner: %q", out.String())
	}
}

func TestRun_UpSkippedPhases(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.SkipRegistry, cfg.SkipBuild, cfg.SkipQuakePush = true, true, true
	// Health will fail because we have no servers, but we drive the up path
	// far enough to cover the [skip] branches + the health-failure teardown.
	cfg.HealthTimeout = 50 * time.Millisecond
	r := newFakeRunner()
	sigCh := make(chan os.Signal, 1)
	if err := Run(context.Background(), cfg, r, sigCh); err == nil {
		t.Fatal("expected health err")
	}
	for _, want := range []string{"[skip] registry", "[skip] build", "[skip] quake-push"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in: %s", want, out.String())
		}
	}
}

func TestRun_UpRegistryFails(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort = 65520
	r := newFakeRunner()
	r.runErr["docker"] = errors.New("docker missing")
	sigCh := make(chan os.Signal, 1)
	if err := Run(context.Background(), cfg, r, sigCh); err == nil {
		t.Fatal("expected err")
	}
}

func TestRun_UpBuildFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort = portOf(srv.URL)
	r := newFakeRunner()
	r.runErr["task"] = errors.New("build fail")
	sigCh := make(chan os.Signal, 1)
	if err := Run(context.Background(), cfg, r, sigCh); err == nil {
		t.Fatal("expected err")
	}
}

func TestRun_UpQuakePushNonFatal(t *testing.T) {
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		return srv, portOf(srv.URL)
	}
	sReg, pReg := mk()
	sBox, pBox := mk()
	sAqua, pAqua := mk()
	sLogin, pLogin := mk()
	defer sReg.Close()
	defer sBox.Close()
	defer sAqua.Close()
	defer sLogin.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort, cfg.WasmboxPort, cfg.WasmaquaPort, cfg.WasmloginPort = pReg, pBox, pAqua, pLogin
	cfg.HealthTimeout = 500 * time.Millisecond
	cfg.StopGrace = 50 * time.Millisecond
	// Create a fake pak so PhaseQuakePush actually invokes the runner.
	pak := filepath.Join(cfg.EngineDir, "id1", "pak0.pak")
	if err := os.MkdirAll(filepath.Dir(pak), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pak, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newFakeRunner()
	r.runErr["task"] = errors.New("pack fail")
	// task is shared by build + quake; clearing the error only for quake by
	// using a counter is overkill -- we test the non-fatal branch in
	// PhaseQuakePush directly above. Here we set the runErr only after build:
	// use a wrapper that lets build succeed, quake fail.
	r2 := &phasedRunner{buildOK: true, quakeFail: true}
	sigCh := make(chan os.Signal, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(out.String(), "wasmdesk stack up") {
				sigCh <- syscall.SIGTERM
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		sigCh <- syscall.SIGTERM
	}()
	if err := Run(context.Background(), cfg, r2, sigCh); err != nil {
		t.Fatalf("Run err: %v -- out: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "quake push phase (best-effort)") {
		t.Fatalf("expected best-effort warning, got %s", out.String())
	}
}

// phasedRunner: succeeds for build tasks, fails for quake tasks, succeeds for
// everything else.
type phasedRunner struct {
	fakeRunner
	buildOK, quakeFail bool
	calls              atomic.Int32
}

func (p *phasedRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	n := p.calls.Add(1)
	// Calls 1..3 = build (wasmbox build:all, wasmaqua build, wasmlogin build)
	// Call 4 = quake oci-pack
	if n >= 4 && p.quakeFail {
		return errors.New("quake fail")
	}
	return nil
}
func (p *phasedRunner) Spawn(ctx context.Context, dir string, env []string, name string, args ...string) (Proc, error) {
	return newFakeProc(name), nil
}

func TestRun_UpHealthFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort = portOf(srv.URL) // registry reuse path
	cfg.HealthTimeout = 50 * time.Millisecond
	r := newFakeRunner()
	sigCh := make(chan os.Signal, 1)
	if err := Run(context.Background(), cfg, r, sigCh); err == nil {
		t.Fatal("expected health err")
	}
}

func TestRun_UpSpawnFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort = portOf(srv.URL)
	r := newFakeRunner()
	r.spawnEr[filepath.Join(cfg.WasmboxDir, "bin", "wasmbox-serve")] = errors.New("nope")
	sigCh := make(chan os.Signal, 1)
	if err := Run(context.Background(), cfg, r, sigCh); err == nil {
		t.Fatal("expected err")
	}
}

func TestRun_UpContextCancelled(t *testing.T) {
	// Set up 4 endpoints so health passes, then cancel the context to drive
	// the <-ctx.Done() branch.
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		return srv, portOf(srv.URL)
	}
	sReg, pReg := mk()
	sBox, pBox := mk()
	sAqua, pAqua := mk()
	sLogin, pLogin := mk()
	defer sReg.Close()
	defer sBox.Close()
	defer sAqua.Close()
	defer sLogin.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort, cfg.WasmboxPort, cfg.WasmaquaPort, cfg.WasmloginPort = pReg, pBox, pAqua, pLogin
	cfg.HealthTimeout = 500 * time.Millisecond
	cfg.StopGrace = 50 * time.Millisecond
	r := newFakeRunner()
	sigCh := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if strings.Contains(out.String(), "wasmdesk stack up") {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := Run(ctx, cfg, r, sigCh); err != nil {
		t.Fatal(err)
	}
}

func TestRun_UpChildExits(t *testing.T) {
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		return srv, portOf(srv.URL)
	}
	sReg, pReg := mk()
	sBox, pBox := mk()
	sAqua, pAqua := mk()
	sLogin, pLogin := mk()
	defer sReg.Close()
	defer sBox.Close()
	defer sAqua.Close()
	defer sLogin.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort, cfg.WasmboxPort, cfg.WasmaquaPort, cfg.WasmloginPort = pReg, pBox, pAqua, pLogin
	cfg.HealthTimeout = 500 * time.Millisecond
	cfg.StopGrace = 50 * time.Millisecond
	r := newFakeRunner()
	sigCh := make(chan os.Signal, 1)
	go func() {
		// Wait for spawn to complete, then signal one child to exit.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			r.mu.Lock()
			n := len(r.procs)
			r.mu.Unlock()
			if n == 3 && strings.Contains(out.String(), "wasmdesk stack up") {
				_ = r.procs[0].Signal(syscall.SIGTERM) // triggers Wait() to return -> WaitAny fires
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := Run(context.Background(), cfg, r, sigCh); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "child process exited unexpectedly") {
		t.Fatalf("expected child-exit branch, got %s", out.String())
	}
}

func TestRun_Down(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.Mode = "down"
	r := newFakeRunner()
	if err := Run(context.Background(), cfg, r, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) == 0 {
		t.Fatal("expected down to invoke pkill+docker")
	}
}

func TestRun_StatusAllUp(t *testing.T) {
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		return srv, portOf(srv.URL)
	}
	s1, p1 := mk()
	s2, p2 := mk()
	s3, p3 := mk()
	s4, p4 := mk()
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	defer s4.Close()
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort, cfg.WasmboxPort, cfg.WasmaquaPort, cfg.WasmloginPort = p1, p2, p3, p4
	cfg.Mode = "status"
	if err := Run(context.Background(), cfg, newFakeRunner(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("expected OK output, got %q", out.String())
	}
}

func TestRun_StatusSomeDown(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.RegistryPort, cfg.WasmboxPort, cfg.WasmaquaPort, cfg.WasmloginPort = 65521, 65522, 65523, 65524
	cfg.Mode = "status"
	if err := Run(context.Background(), cfg, newFakeRunner(), nil); err == nil {
		t.Fatal("expected err when endpoints down")
	}
}

func TestRun_UnknownMode(t *testing.T) {
	var out safeBuf
	cfg := baseCfg(t, &out)
	cfg.Mode = "wat"
	if err := Run(context.Background(), cfg, newFakeRunner(), nil); err == nil {
		t.Fatal("expected err")
	}
}

// ---- main() shim ----

func TestMain_BadFlag(t *testing.T) {
	saveArgs, saveExit := os.Args, osExit
	defer func() { os.Args, osExit = saveArgs, saveExit }()
	os.Args = []string{"wasmdesk-up", "-not-a-flag"}
	var code int
	osExit = func(c int) { code = c; panic("exit") }
	defer func() { recover() }()
	main()
	_ = code
}

func TestMain_RunFails(t *testing.T) {
	saveArgs, saveExit := os.Args, osExit
	defer func() { os.Args, osExit = saveArgs, saveExit }()
	os.Args = []string{"wasmdesk-up", "-mode=wat"}
	var code int
	osExit = func(c int) { code = c; panic("exit1") }
	defer func() { recover() }()
	main()
	_ = code
}

func TestMain_StatusOK_Path(t *testing.T) {
	// Drive main() through a successful status. Spin up 4 servers, set env
	// to point at them, run main with -mode=status. osExit must not be
	// called.
	mk := func() (*httptest.Server, int) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		return srv, portOf(srv.URL)
	}
	s1, p1 := mk()
	s2, p2 := mk()
	s3, p3 := mk()
	s4, p4 := mk()
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	defer s4.Close()
	saveArgs, saveExit := os.Args, osExit
	defer func() { os.Args, osExit = saveArgs, saveExit }()
	os.Args = []string{"wasmdesk-up", "-mode=status",
		fmt.Sprintf("-registry-port=%d", p1),
		fmt.Sprintf("-wasmbox-port=%d", p2),
		fmt.Sprintf("-wasmaqua-port=%d", p3),
		fmt.Sprintf("-wasmlogin-port=%d", p4),
	}
	exited := false
	osExit = func(c int) { exited = true }
	main()
	if exited {
		t.Fatal("osExit should not fire on success")
	}
}

// ---- logf nil-writer ----

func TestLogf_NilWriter(t *testing.T) {
	logf(nil, "ignored")
}

func TestSyncWriter_Nil(t *testing.T) {
	sw := &syncWriter{w: nil}
	n, err := sw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("nil writer should swallow, got n=%d err=%v", n, err)
	}
}

// safeBuf is a bytes.Buffer with a mutex; tests read the buffer concurrently
// with the supervisor's writer goroutines so we need locked access on both
// sides. (cfg.Stdout is wrapped in a syncWriter by Run() itself, so the write
// side of that side is safe; this just makes the read side equally safe.)
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// ---- helpers used by tests ----

func portOf(rawURL string) int {
	// rawURL is e.g. http://127.0.0.1:53827
	i := strings.LastIndex(rawURL, ":")
	if i < 0 {
		return 0
	}
	var p int
	fmt.Sscanf(rawURL[i+1:], "%d", &p)
	return p
}
