// live.go - the live-control client: a localhost TCP, line-delimited-JSON socket to a ControlListener
// (cpp/ControlListener.h) embedded in a host that has a plugin loaded. set_param / get_param / play_note /
// all_notes_off forward over this socket to the running instance. The wire protocol mirrors ControlListener
// 1:1 (one JSON object per line, one JSON reply per line, with an {ok:bool} field). This client is a thin
// forwarder that also satisfies the LiveEndpoint interface the generic tools depend on.

package sidechain

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultPort is the ControlListener's default loopback port (matches cpp/ControlListener.h kDefaultPort and
// the SIDECHAIN_PORT env bring-up).
const DefaultPort = 51703

// LiveEndpoint is the transport the generic param tools drive when connected to a running instance. One impl
// (*liveClient) speaks the ControlListener wire protocol; a host may provide its own.
type LiveEndpoint interface {
	SetParam(id string, normalized float64) (value, applied float64, text string, err error)
	GetParam(id string) (value, normalized float64, text string, err error)
	PlayNote(note, chn int, vel float64) error
	NoteOff(note, chn int) error
	AllNotesOff() error
	ResetInit() error
	GetFullState() (xml string, err error)
	LoadState(xml string) error
	Close()
}

// liveClient is a thread-safe request/response wrapper over one control socket. Its own request path is
// serialized by the caller (the session mutex), so the line-delimited protocol never interleaves.
type liveClient struct {
	conn  net.Conn
	rd    *bufio.Reader
	host  string
	port  int
	reqID int
}

// dialLive connects to the ControlListener and handshakes with a ping. Bound to the caller-supplied host
// (default 127.0.0.1); the listener only ever binds loopback, so a remote host simply fails to dial.
func dialLive(host string, port int) (*liveClient, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	lc := &liveClient{conn: conn, rd: bufio.NewReader(conn), host: host, port: port}
	if _, err := lc.request(map[string]any{"cmd": "ping"}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}
	return lc, nil
}

// request sends one command and returns the decoded reply. Adds a per-request id for correlation and a
// deadline so a wedged host can't hang a tool call forever.
func (lc *liveClient) request(obj map[string]any) (map[string]any, error) {
	if lc == nil || lc.conn == nil {
		return nil, errors.New("not connected")
	}
	lc.reqID++
	obj["id"] = lc.reqID
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	lc.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := lc.conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := lc.rd.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("bad reply: %w", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		return resp, fmt.Errorf("host: %v", resp["error"])
	}
	return resp, nil
}

// ---- LiveEndpoint impl ----

func (lc *liveClient) SetParam(id string, normalized float64) (value, applied float64, text string, err error) {
	resp, err := lc.request(map[string]any{"cmd": "set_param", "param": id, "normalized": normalized})
	if err != nil {
		return 0, 0, "", err
	}
	applied, _ = resp["normalized"].(float64)
	value, _ = resp["value"].(float64)
	text, _ = resp["text"].(string)
	return value, applied, text, nil
}

func (lc *liveClient) GetParam(id string) (value, normalized float64, text string, err error) {
	resp, err := lc.request(map[string]any{"cmd": "get_param", "param": id})
	if err != nil {
		return 0, 0, "", err
	}
	value, _ = resp["value"].(float64)
	normalized, _ = resp["normalized"].(float64)
	text, _ = resp["text"].(string)
	return value, normalized, text, nil
}

func (lc *liveClient) PlayNote(note, chn int, vel float64) error {
	_, err := lc.request(map[string]any{"cmd": "note_on", "note": note, "vel": vel, "chan": chn})
	return err
}

func (lc *liveClient) NoteOff(note, chn int) error {
	_, err := lc.request(map[string]any{"cmd": "note_off", "note": note, "chan": chn})
	return err
}

func (lc *liveClient) AllNotesOff() error {
	_, err := lc.request(map[string]any{"cmd": "all_notes_off"})
	return err
}

func (lc *liveClient) ResetInit() error {
	_, err := lc.request(map[string]any{"cmd": "reset_init"})
	return err
}

func (lc *liveClient) GetFullState() (string, error) {
	resp, err := lc.request(map[string]any{"cmd": "get_full_state"})
	if err != nil {
		return "", err
	}
	xml, _ := resp["xml"].(string)
	return xml, nil
}

func (lc *liveClient) LoadState(xml string) error {
	_, err := lc.request(map[string]any{"cmd": "load_state", "xml": xml})
	return err
}

func (lc *liveClient) Close() {
	if lc != nil && lc.conn != nil {
		lc.conn.Close()
		lc.conn = nil
	}
}

// ---- live tools ----

type connectLiveIn struct {
	Host string `json:"host,omitempty" jsonschema:"host running the ControlListener (default 127.0.0.1; the listener only binds loopback)"`
	Port int    `json:"port,omitempty" jsonschema:"listen port (default 51703; matches SIDECHAIN_PORT and cpp/ControlListener kDefaultPort)"`
}

func (s *session) handleConnectLive(ctx context.Context, _ *mcp.CallToolRequest, in connectLiveIn) (*mcp.CallToolResult, any, error) {
	host := orDefault(in.Host, "127.0.0.1")
	port := in.Port
	if port == 0 {
		port = DefaultPort
	}
	lc, err := dialLive(host, port)
	if err != nil {
		return textResult(fmt.Sprintf("connect_live failed (%s:%d): %v. Is a Sidechain host running with a plugin loaded and control enabled?", host, port, err)), nil, nil
	}
	s.mu.Lock()
	if s.live != nil {
		s.live.Close()
	}
	s.live = lc
	s.mu.Unlock()
	return textResult(fmt.Sprintf("Connected LIVE to %s:%d. set_param / get_param now drive the RUNNING instance; play_note plays it; all_notes_off panics it.", host, port)), nil, nil
}

func (s *session) handleDisconnectLive(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.live = nil
	s.mu.Unlock()
	if lc == nil {
		return textResult("disconnect_live: was not live."), nil, nil
	}
	lc.Close()
	return textResult("Disconnected from the live instance."), nil, nil
}

type playNoteIn struct {
	Note   int     `json:"note" jsonschema:"MIDI note number 0..127 (60 = middle C)"`
	Vel    float64 `json:"vel,omitempty" jsonschema:"velocity 0..1 (default 0.8)"`
	Chan   int     `json:"chan,omitempty" jsonschema:"MIDI channel 1..16 (default 1)"`
	HoldMs int     `json:"holdMs,omitempty" jsonschema:"if >0, auto-release after this many ms; else leaves the note on (release with all_notes_off or a note with holdMs)"`
}

func (s *session) handlePlayNote(ctx context.Context, _ *mcp.CallToolRequest, in playNoteIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("play_note: not live. Call connect_live first."), nil, nil
	}
	ch := in.Chan
	if ch == 0 {
		ch = 1
	}
	vel := in.Vel
	if vel == 0 {
		vel = 0.8
	}
	if err := lc.PlayNote(in.Note, ch, vel); err != nil {
		return textResult("play_note failed: " + err.Error()), nil, nil
	}
	if in.HoldMs > 0 {
		time.Sleep(time.Duration(in.HoldMs) * time.Millisecond)
		if err := lc.NoteOff(in.Note, ch); err != nil {
			return textResult("play_note: note_on ok but note_off failed: " + err.Error()), nil, nil
		}
		return textResult(fmt.Sprintf("Played note %d (vel %.2f, ch %d) held %d ms.", in.Note, vel, ch, in.HoldMs)), nil, nil
	}
	return textResult(fmt.Sprintf("Note %d ON (vel %.2f, ch %d). Release with all_notes_off or play_note holdMs.", in.Note, vel, ch)), nil, nil
}

func (s *session) handleAllNotesOff(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("all_notes_off: not live. Call connect_live first."), nil, nil
	}
	if err := lc.AllNotesOff(); err != nil {
		return textResult("all_notes_off failed: " + err.Error()), nil, nil
	}
	return textResult("All notes off (live panic)."), nil, nil
}

// registerLiveTools wires the live verbs. set_param/get_param forwarding is inside their handlers (they check
// s.live), so only the live-specific verbs are registered here.
func registerLiveTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "connect_live", Description: "Connect to a RUNNING Sidechain host (localhost) that has a plugin loaded, so set_param/get_param drive the live instance and play_note plays it."}, s.handleConnectLive)
	mcp.AddTool(srv, &mcp.Tool{Name: "disconnect_live", Description: "Disconnect from the live instance."}, s.handleDisconnectLive)
	mcp.AddTool(srv, &mcp.Tool{Name: "play_note", Description: "Play a MIDI note on the live instance (note, velocity, channel; optional holdMs to auto-release). Live only."}, s.handlePlayNote)
	mcp.AddTool(srv, &mcp.Tool{Name: "all_notes_off", Description: "Release all notes on the live instance (panic). Live only."}, s.handleAllNotesOff)
}
