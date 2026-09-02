// managed_test.go - unit + end-to-end tests for managed-host mode WITHOUT a real JUCE host. The unit tests
// cover free-port selection, host-binary discovery, the mutually-exclusive/neither --plugin/--catalog
// validation, and the catalog-wait timeout. The end-to-end test compiles a tiny FAKE host binary (a standalone
// program that speaks the same line-JSON ControlServer protocol as live_test.go's fakeHost, writes a minimal
// catalog, then serves the socket), points managed startup at it via --host-bin, and asserts startup connects
// and enumerates.

package sidechain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("freePort returned %d, want a valid TCP port", p)
	}
	// resolvePort passes a non-zero port through unchanged.
	if got, _ := resolvePort(51703); got != 51703 {
		t.Fatalf("resolvePort(51703) = %d, want it unchanged", got)
	}
	// resolvePort(0) picks a fresh one.
	if got, err := resolvePort(0); err != nil || got <= 0 {
		t.Fatalf("resolvePort(0) = %d, %v", got, err)
	}
}

func TestFindHostBinary(t *testing.T) {
	dir := t.TempDir()

	// --host-bin: explicit path that exists is returned verbatim.
	explicit := filepath.Join(dir, "my-host")
	writeExecutable(t, explicit)
	if got, err := findHostBinary(explicit); err != nil || got != explicit {
		t.Fatalf("findHostBinary(explicit) = %q, %v; want %q", got, err, explicit)
	}

	// --host-bin: explicit path that does NOT exist is an error.
	if _, err := findHostBinary(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("findHostBinary(missing explicit) should error")
	}

	// beside-exe discovery: place a sidechain-host next to the test binary's os.Executable dir and confirm it is
	// found ahead of PATH. We cannot move os.Executable, so assert the discovered path is either beside-exe or on
	// PATH; here we specifically test the not-found error by clearing PATH and using a name unlikely beside-exe.
	t.Run("not_found", func(t *testing.T) {
		t.Setenv("PATH", dir) // dir has "my-host", not "sidechain-host"
		if _, err := findHostBinary(""); err == nil {
			t.Fatal("findHostBinary(discovery) should error when the host binary is nowhere")
		} else if !strings.Contains(err.Error(), "cannot find") {
			t.Fatalf("unexpected discovery error: %v", err)
		}
	})

	// PATH discovery: put a sidechain-host on PATH and confirm it is found.
	t.Run("on_path", func(t *testing.T) {
		pathDir := t.TempDir()
		onPath := filepath.Join(pathDir, hostBinName())
		writeExecutable(t, onPath)
		t.Setenv("PATH", pathDir)
		got, err := findHostBinary("")
		if err != nil {
			t.Fatalf("findHostBinary(PATH) err: %v", err)
		}
		// LookPath may return an absolute or PATH-relative form; resolve both to compare.
		if filepath.Base(got) != hostBinName() {
			t.Fatalf("findHostBinary(PATH) = %q, want a %s", got, hostBinName())
		}
	})
}

func TestRunModeValidation(t *testing.T) {
	// Both set: error.
	if err := Run(context.Background(), Config{Plugin: "x.vst3", CatalogPath: "c.json"}); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both --plugin and --catalog should be mutually exclusive, got %v", err)
	}
	// Neither set: error.
	if err := Run(context.Background(), Config{}); err == nil ||
		!strings.Contains(err.Error(), "either --plugin") {
		t.Fatalf("neither --plugin nor --catalog should error, got %v", err)
	}
}

func TestWaitForCatalogTimeout(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "never.json")
	start := time.Now()
	err := waitForCatalog(context.Background(), missing, nil, nil, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitForCatalog should time out, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("waitForCatalog returned too early (%s), timeout not honored", elapsed)
	}

	// A file that appears non-empty resolves before the deadline.
	present := filepath.Join(dir, "here.json")
	if err := os.WriteFile(present, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForCatalog(context.Background(), present, nil, nil, time.Second); err != nil {
		t.Fatalf("waitForCatalog(present) = %v, want nil", err)
	}
}

// TestManagedStartupWithFakeHost is the end-to-end managed-startup test: it compiles a standalone fake host,
// points startManaged at it via --host-bin, and asserts the server spawns it, loads its catalog, and
// auto-connects the live endpoint (enumerate > 0, read + set round-trip).
func TestManagedStartupWithFakeHost(t *testing.T) {
	bin := buildFakeHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mh, srv, err := startManaged(ctx, Config{
		Plugin:  "/fake/Plugin.vst3",
		HostBin: bin,
		Port:    0,
		Name:    "test",
		Version: "0.0.0",
	})
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	defer mh.shutdown()
	if srv == nil {
		t.Fatal("startManaged returned a nil server")
	}

	if n := len(mh.catalog.All()); n == 0 {
		t.Fatal("managed startup enumerated 0 params")
	}
	if mh.live == nil {
		t.Fatal("managed startup did not auto-connect the live endpoint")
	}

	// The selfTest path exercises read + set against the connected fake host.
	if err := mh.selfTest(); err != nil {
		t.Fatalf("selfTest against fake host: %v", err)
	}
}

// TestManagedSelfTestRun drives the full Run(SelfTest) path against the fake host: it must return nil (exit 0).
func TestManagedSelfTestRun(t *testing.T) {
	bin := buildFakeHost(t)
	err := Run(context.Background(), Config{
		Plugin:   "/fake/Plugin.vst3",
		HostBin:  bin,
		SelfTest: true,
	})
	if err != nil {
		t.Fatalf("Run selftest against fake host = %v, want nil (exit 0)", err)
	}
}

// writeExecutable creates a 0755 file so os.Stat/LookPath treat it as a runnable binary.
func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

// buildFakeHost writes a tiny standalone Go program that speaks the ControlServer wire protocol (ping/get_param/
// set_param and friends) over a socket, writes a minimal valid catalog, then serves, and compiles it into
// t.TempDir(). It returns the binary path. This is the JUCE-free stand-in for cpp/Host used by the managed
// end-to-end tests: same bytes on the socket as live_test.go's fakeHost.
func buildFakeHost(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping fake-host build")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeHostProgram), 0o600); err != nil {
		t.Fatalf("write fake host source: %v", err)
	}
	out := filepath.Join(dir, hostBinName())
	build := exec.Command("go", "build", "-o", out, src)
	build.Env = os.Environ()
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake host: %v\n%s", err, b)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(out, 0o755)
	}
	return out
}

// fakeHostProgram is a self-contained main that mirrors the cpp host's CLI + wire protocol: parse
// --plugin/--catalog/--port, write a minimal catalog JSON, then open the control socket AFTER (so the catalog
// appears slightly before the socket, exactly like the real host). The protocol logic is the same shape as
// live_test.go's fakeHost.
const fakeHostProgram = `package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
)

func main() {
	plugin := flag.String("plugin", "", "")
	catalog := flag.String("catalog", "plugin-catalog.json", "")
	port := flag.Int("port", 0, "")
	flag.Parse()
	_ = plugin

	// Write a minimal valid catalog (one float param) BEFORE opening the socket, mirroring the real host order.
	cat := map[string]any{
		"stateRootTag": "PARAMS",
		"stateVersion": 1,
		"plugin":       map[string]any{"name": "Fake", "manufacturer": "Test", "format": "VST3"},
		"count":        1,
		"params": []map[string]any{
			{"id": "cutoff", "label": "Cutoff", "group": "filter", "type": "float", "min": 20.0, "max": 20000.0, "default": 1000.0},
		},
	}
	b, _ := json.Marshal(cat)
	if err := os.WriteFile(*catalog, b, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake-host: write catalog: %v\n", err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-host: listen: %v\n", err)
		os.Exit(1)
	}
	params := map[string]float64{}
	client := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		client++
		go func(conn net.Conn, cid int) {
			defer conn.Close()
			rd := bufio.NewReader(conn)
			for {
				line, err := rd.ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				if json.Unmarshal(line, &req) != nil {
					continue
				}
				resp := map[string]any{"ok": true}
				switch req["cmd"] {
				case "ping":
					resp["pong"] = true
					resp["client"] = cid
				case "set_param":
					id, _ := req["param"].(string)
					if n, ok := req["normalized"].(float64); ok {
						params[id] = n
					}
					resp["param"] = id
					resp["normalized"] = params[id]
					resp["value"] = params[id]
					resp["text"] = "ok"
				case "get_param":
					id, _ := req["param"].(string)
					resp["param"] = id
					resp["normalized"] = params[id]
					resp["value"] = params[id]
					resp["text"] = "ok"
				default:
					resp["ok"] = false
					resp["error"] = "unknown_cmd"
				}
				if id, ok := req["id"]; ok {
					resp["id"] = id
				}
				out, _ := json.Marshal(resp)
				conn.Write(append(out, '\n'))
			}
		}(conn, client)
	}
}
`
