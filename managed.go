// managed.go - managed-host mode: the Go server SPAWNS and SUPERVISES the cpp/Host subprocess so the whole
// bridge is ONE command (what an MCP client wants). Instead of the user starting sidechain-host by hand,
// writing a catalog, and the agent calling connect_live, `sidechain --plugin <path>` does all of it: pick a
// port, spawn the host, wait for its catalog, load it, auto-connect the live endpoint, then serve MCP on stdio.
//
// The two-process split is unchanged (no JUCE is linked into Go); this just automates the wiring. --selftest
// runs the same startup then verifies it (enumerate/connect/read/set) and exits, which is what CI runs against
// the shipped binary.

package sidechain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blackwell-systems/mcp-vst-sidechain/internal/hostbin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hostBinName is the cpp host executable's base name (with the platform's exe suffix).
func hostBinName() string {
	if runtime.GOOS == "windows" {
		return "sidechain-host.exe"
	}
	return "sidechain-host"
}

// catalogWaitTimeout bounds how long managed startup waits for the host to write its catalog before failing.
const catalogWaitTimeout = 30 * time.Second

// runManaged is the managed-mode entry point (Config.Plugin is set). It brings the host up, then either serves
// MCP over stdio (normal) or runs the self-test and returns (SelfTest). The host subprocess and the temp catalog
// are always cleaned up on the way out.
func runManaged(ctx context.Context, cfg Config) error {
	mh, srv, err := startManaged(ctx, cfg)
	if err != nil {
		return err
	}
	defer mh.shutdown()

	if cfg.SelfTest {
		if err := mh.selfTest(); err != nil {
			fmt.Fprintf(os.Stderr, "sidechain: selftest FAILED: %v\n", err)
			return err
		}
		fmt.Fprintf(os.Stderr, "sidechain: selftest OK (plugin %s, %d params, live read+set verified)\n", cfg.Plugin, len(mh.catalog.All()))
		return nil
	}

	// Serve MCP over stdio. Install a signal handler so SIGINT/SIGTERM triggers a clean host teardown (the
	// deferred shutdown), and watch for the host dying under us so we log it rather than failing silently.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go mh.watch(ctx)

	return srv.Run(ctx, &mcp.StdioTransport{})
}

// managedHost owns a spawned cpp host subprocess plus the resources managed startup created for it (the temp
// catalog file, the live client). shutdown terminates the subprocess cleanly and removes the temp file.
type managedHost struct {
	cmd         *exec.Cmd
	catalogPath string // temp catalog file we told the host to write; removed on shutdown
	catalog     *Catalog
	live        *liveClient

	stderr *stderrTail // ring buffer of the host's stderr, for error messages

	shutdownOnce sync.Once
}

// startManaged performs the managed bring-up: pick port, spawn host, wait for catalog, load it, build the
// server, auto-connect the live endpoint. On any failure it tears down whatever it already started.
func startManaged(ctx context.Context, cfg Config) (*managedHost, *mcp.Server, error) {
	port, err := resolvePort(cfg.Port)
	if err != nil {
		return nil, nil, err
	}

	bin, err := findHostBinary(cfg.HostBin)
	if err != nil {
		return nil, nil, err
	}

	catalogPath, err := tempCatalogPath()
	if err != nil {
		return nil, nil, err
	}

	mh := &managedHost{catalogPath: catalogPath, stderr: newStderrTail(64)}

	// #nosec G204 - args are the operator-supplied plugin path and our own temp path/port, not agent input.
	mh.cmd = exec.CommandContext(ctx, bin, "--plugin", cfg.Plugin, "--catalog", catalogPath, "--port", strconv.Itoa(port))
	mh.cmd.Stdout = os.Stderr // keep the host off our stdio (that is the MCP transport)
	mh.cmd.Stderr = io.MultiWriter(&prefixWriter{w: os.Stderr, prefix: []byte("sidechain-host: ")}, mh.stderr)
	if err := mh.cmd.Start(); err != nil {
		_ = os.Remove(catalogPath)
		return nil, nil, fmt.Errorf("spawn host %s: %w", bin, err)
	}
	fmt.Fprintf(os.Stderr, "sidechain: spawned host %s (pid %d, port %d)\n", bin, mh.cmd.Process.Pid, port)

	// Wait for the catalog to appear and be non-empty. The host writes it just BEFORE opening the socket, so this
	// is the readiness signal for the load; the socket dial below then retries briefly for the small lag.
	if err := waitForCatalog(ctx, catalogPath, mh.cmd, mh.stderr, catalogWaitTimeout); err != nil {
		mh.shutdown()
		return nil, nil, err
	}

	mh.catalog, err = loadCatalogFile(catalogPath)
	if err != nil {
		mh.shutdown()
		return nil, nil, err
	}
	fmt.Fprintf(os.Stderr, "sidechain: loaded catalog (%d params) from spawned host\n", len(mh.catalog.All()))

	srv, s := NewServer(cfg.Name, cfg.Version, mh.catalog)
	attachStore(s, cfg.SemanticDir)

	// Auto-connect the live endpoint the same way connect_live does, retrying briefly since the socket may lag
	// the catalog by a moment.
	lc, err := dialLiveRetry("127.0.0.1", port, 5*time.Second)
	if err != nil {
		mh.shutdown()
		return nil, nil, fmt.Errorf("auto-connect live 127.0.0.1:%d: %w%s", port, err, mh.stderr.tailNote())
	}
	mh.live = lc
	s.mu.Lock()
	s.live = lc
	s.reloadSemanticsLocked()
	s.mu.Unlock()
	fmt.Fprintf(os.Stderr, "sidechain: live-connected to spawned host 127.0.0.1:%d\n", port)

	return mh, srv, nil
}

// watch logs if the host subprocess exits while we are serving (so an operator sees why set/get_param suddenly
// fail). It returns when the host exits or the context is cancelled.
func (mh *managedHost) watch(ctx context.Context) {
	done := make(chan error, 1)
	go func() { done <- mh.cmd.Wait() }()
	select {
	case err := <-done:
		select {
		case <-ctx.Done(): // we are already shutting down; the exit is expected
		default:
			fmt.Fprintf(os.Stderr, "sidechain: host exited unexpectedly (%v). Live control is down.%s\n", err, mh.stderr.tailNote())
		}
	case <-ctx.Done():
	}
}

// selfTest verifies the managed bring-up end to end: the catalog enumerated params, the live endpoint is
// connected, and one param round-trips (read then set). Returns nil on success.
func (mh *managedHost) selfTest() error {
	if n := len(mh.catalog.All()); n == 0 {
		return errors.New("catalog enumerated 0 params")
	}
	if mh.live == nil {
		return errors.New("live endpoint not connected")
	}
	// Pick a settable param: prefer a continuous one, else the first.
	target := mh.catalog.All()[0]
	for _, p := range mh.catalog.All() {
		if p.Type == "float" {
			target = p
			break
		}
	}
	if _, _, _, err := mh.live.GetParam(target.ID); err != nil {
		return fmt.Errorf("read param %q: %w", target.ID, err)
	}
	if _, _, _, err := mh.live.SetParam(target.ID, 0.5, false); err != nil {
		return fmt.Errorf("set param %q: %w", target.ID, err)
	}
	return nil
}

// shutdown terminates the host subprocess cleanly (SIGTERM, then SIGKILL after a grace period) and removes the
// temp catalog. Idempotent.
func (mh *managedHost) shutdown() {
	mh.shutdownOnce.Do(func() {
		if mh.live != nil {
			mh.live.Close()
		}
		if mh.cmd != nil && mh.cmd.Process != nil {
			terminate(mh.cmd, 3*time.Second)
		}
		if mh.catalogPath != "" {
			_ = os.Remove(mh.catalogPath)
		}
	})
}

// terminate asks the process to exit (SIGTERM on unix; Kill on Windows which has no SIGTERM), then hard-kills it
// if it has not exited within grace.
func terminate(cmd *exec.Cmd, grace time.Duration) {
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()

	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-done
	}
}

// resolvePort returns port unchanged when non-zero; when 0 it binds 127.0.0.1:0 to claim a free ephemeral port,
// reads it, closes the listener, and returns it (there is a small race until the host rebinds it, acceptable for
// loopback bring-up).
func resolvePort(port int) (int, error) {
	if port != 0 {
		return port, nil
	}
	return freePort()
}

// freePort asks the OS for an unused loopback TCP port by binding :0 and reading back the assigned port.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// findHostBinary locates the sidechain-host executable. If explicit is set it is used verbatim (error if it does
// not exist). Otherwise discovery order is: an embedded host (a single-file `-tags embedhost` build, extracted to a
// cache dir), then next to the running sidechain executable, then on PATH.
func findHostBinary(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--host-bin %s: %w", explicit, err)
		}
		return explicit, nil
	}

	// A shipped single-file build carries the host embedded; extract and use it. An explicit --host-bin above still
	// wins (for development against a freshly built host).
	if p, embedded, err := hostbin.Extract(); embedded {
		if err != nil {
			return "", fmt.Errorf("extract embedded host: %w", err)
		}
		return p, nil
	}

	name := hostBinName()
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("cannot find the host binary %q (looked next to this executable and on PATH); pass --host-bin <path>", name)
}

// tempCatalogPath returns a fresh, non-existent path in the temp dir for the host to write its catalog to. It
// creates the file (so the parent dir is writable and the path is unique), then removes it so waitForCatalog's
// non-empty check is meaningful (an empty just-created file must not read as ready).
func tempCatalogPath() (string, error) {
	f, err := os.CreateTemp("", "sidechain-catalog-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp catalog path: %w", err)
	}
	path := f.Name()
	f.Close()
	_ = os.Remove(path)
	return path, nil
}

// waitForCatalog blocks until path exists and is non-empty, the timeout elapses, or the host exits first. On
// timeout or early host exit it returns an error that includes the tail of the host's stderr. A nil cmd skips
// the early-exit watch (used by tests that stat a path with no subprocess).
func waitForCatalog(ctx context.Context, path string, cmd *exec.Cmd, tail *stderrTail, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	// Detect the host exiting before it ever wrote the catalog.
	var exited chan error
	if cmd != nil {
		exited = make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
	}

	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("host exited before writing catalog (%v)%s", err, tail.tailNote())
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out after %s waiting for host to write catalog %s%s", timeout, path, tail.tailNote())
		case <-tick.C:
		}
	}
}

// dialLiveRetry dials the live endpoint, retrying on failure until the deadline (the socket may lag the catalog
// by a moment). It reuses dialLive so the handshake/reader are identical to connect_live.
func dialLiveRetry(host string, port int, within time.Duration) (*liveClient, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for {
		lc, err := dialLive(host, port)
		if err == nil {
			return lc, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stderrTail is a bounded ring buffer capturing the last maxLines lines of the host's stderr so failure messages
// can quote what the host complained about. Writes are concurrent (the copy goroutine) so it is mutex-guarded.
type stderrTail struct {
	mu       sync.Mutex
	maxLines int
	lines    []string
	partial  []byte // bytes since the last newline
}

func newStderrTail(maxLines int) *stderrTail { return &stderrTail{maxLines: maxLines} }

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		t.push(string(t.partial[:i]))
		t.partial = t.partial[i+1:]
	}
	return len(p), nil
}

func (t *stderrTail) push(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > t.maxLines {
		t.lines = t.lines[len(t.lines)-t.maxLines:]
	}
}

// tailNote returns the captured stderr as a parenthetical suffix for an error message (empty if nothing was
// captured). It includes any trailing partial line. Safe on a nil receiver (returns "").
func (t *stderrTail) tailNote() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	all := append([]string{}, t.lines...)
	if len(t.partial) > 0 {
		all = append(all, string(t.partial))
	}
	joined := strings.TrimSpace(strings.Join(all, "\n"))
	if joined == "" {
		return ""
	}
	return " -- host stderr:\n" + joined
}

// prefixWriter prefixes every line it forwards, so the host's stderr is visibly attributed in the merged stream.
type prefixWriter struct {
	w       io.Writer
	prefix  []byte
	midLine bool // last write did not end with a newline, so the next chunk continues a line (no re-prefix)
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		if !p.midLine {
			if _, err := p.w.Write(p.prefix); err != nil {
				return 0, err
			}
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if _, err := p.w.Write(b); err != nil {
				return 0, err
			}
			p.midLine = true
			break
		}
		if _, err := p.w.Write(b[:i+1]); err != nil {
			return 0, err
		}
		p.midLine = false
		b = b[i+1:]
	}
	return total, nil
}
