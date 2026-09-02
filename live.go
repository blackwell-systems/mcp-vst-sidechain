// live.go - the live-control client: a localhost TCP, line-delimited-JSON socket to a ControlServer
// (cpp/ControlServer.h) embedded in a host that has a plugin loaded. set_param / get_param / play_note /
// all_notes_off forward over this socket to the running instance. The wire protocol mirrors ControlServer
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
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultPort is the ControlServer's default loopback port (matches cpp/ControlServer.h kDefaultPort and
// the SIDECHAIN_PORT env bring-up).
const DefaultPort = 51703

// LiveEndpoint is the transport the generic param tools drive when connected to a running instance. One impl
// (*liveClient) speaks the ControlServer wire protocol; a host may provide its own.
//
// SetParam takes a value plus isReal: when isReal is true, v is a REAL-unit value (only for HasRealRange params)
// and the endpoint forwards it as `value` so the plugin applies its own (possibly skewed) real->normalized
// curve; when false, v is already normalized 0..1 and is forwarded as `normalized`. Keeping the two apart is
// what preserves fidelity on skewed params: the Go layer never linearises a curve it does not own.
type LiveEndpoint interface {
	SetParam(id string, v float64, isReal bool) (value, applied float64, text string, err error)
	GetParam(id string) (value, normalized float64, text string, err error)
	SampleText(id string, points []float64) ([]ValueSample, error) // Phase-1 probe: sweep value text, restore
	PlayNote(note, chn int, vel float64) error
	NoteOff(note, chn int) error
	AllNotesOff() error
	ResetInit() error
	GetFullState() (xml string, err error)
	LoadState(xml string) error

	// Concurrency surface (C2/C3). ClientID is this controller's identity (0 if unknown). Govern applies a
	// governed command (op is acquire_instance/release_instance/acquire_section/release_section; group names the
	// section for the section ops) and returns how the conflict tier resolved it plus the resulting state.
	// GetGoverned reads the current governed state and the leasable sections. DrainEvents returns the buffered
	// server-pushed change events (param_changed / governed_changed) since the last drain, oldest first.
	ClientID() int
	Govern(op, group string) (resolution string, gov GovernedState, err error)
	GetGoverned() (gov GovernedState, sections []string, err error)
	DrainEvents() []map[string]any

	Close()
}

// GovernedState is the C3 coordination state as seen over the wire: who holds the whole-instance edit lease, who
// holds each section (param group) lease, and the monotone patch generation (bumped on a whole-patch change).
type GovernedState struct {
	InstanceLease int            `json:"instance_lease"`
	SectionLeases map[string]int `json:"section_leases"`
	Generation    int            `json:"generation"`
}

// liveClient speaks the (now multiplexed async) control protocol over one socket. The host serves many clients
// and PUSHES unsolicited param_changed events, so replies can no longer be read one-per-request: a background
// reader demultiplexes the socket, routing replies to their waiting caller by request id and events to a
// subscriber channel. Writes are serialized by writeMu; concurrent callers each wait on their own reply channel.
type liveClient struct {
	conn     net.Conn
	host     string
	port     int
	clientID int // controller identity assigned by the host at the ping handshake

	writeMu sync.Mutex // serialize socket writes so two requests never interleave bytes

	pendMu  sync.Mutex
	nextID  int
	pending map[int]chan map[string]any // request id -> reply channel (the reader delivers here)

	events    chan map[string]any // param_changed events (buffered; dropped if nobody drains)
	closed    chan struct{}
	closeOnce sync.Once
}

// dialLive connects to the ControlServer, starts the reader, and handshakes with a ping. Bound to the
// caller-supplied host (default 127.0.0.1); the listener only ever binds loopback, so a remote host fails to dial.
func dialLive(host string, port int) (*liveClient, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	lc := &liveClient{
		conn:    conn,
		host:    host,
		port:    port,
		pending: map[int]chan map[string]any{},
		events:  make(chan map[string]any, 64),
		closed:  make(chan struct{}),
	}
	go lc.readLoop(bufio.NewReader(conn))
	pong, err := lc.request(map[string]any{"cmd": "ping"})
	if err != nil {
		lc.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}
	if cid, ok := pong["client"].(float64); ok { // the host assigns each connection a distinct controller id
		lc.clientID = int(cid)
	}
	return lc, nil
}

// readLoop reads every line and demultiplexes: a message with an "event" field is a server-pushed notification
// (delivered to the events channel, dropped if full); anything else is a reply, routed to its caller by "id".
func (lc *liveClient) readLoop(rd *bufio.Reader) {
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			lc.Close() // unblocks any waiting callers via lc.closed
			return
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if _, isEvent := m["event"]; isEvent {
			select {
			case lc.events <- m:
			default: // no subscriber / full: drop (events are latest-value snapshots)
			}
			continue
		}
		idf, _ := m["id"].(float64)
		lc.pendMu.Lock()
		ch := lc.pending[int(idf)]
		delete(lc.pending, int(idf))
		lc.pendMu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// request sends one command and returns the decoded reply, correlated by id via the reader. A per-request
// timeout keeps a wedged host from hanging a tool call forever.
func (lc *liveClient) request(obj map[string]any) (map[string]any, error) {
	if lc == nil || lc.conn == nil {
		return nil, errors.New("not connected")
	}
	lc.pendMu.Lock()
	lc.nextID++
	id := lc.nextID
	ch := make(chan map[string]any, 1)
	lc.pending[id] = ch
	lc.pendMu.Unlock()
	obj["id"] = id

	b, err := json.Marshal(obj)
	if err != nil {
		lc.cancel(id)
		return nil, err
	}
	lc.writeMu.Lock()
	_, werr := lc.conn.Write(append(b, '\n'))
	lc.writeMu.Unlock()
	if werr != nil {
		lc.cancel(id)
		return nil, werr
	}

	select {
	case resp := <-ch:
		if ok, _ := resp["ok"].(bool); !ok {
			return resp, fmt.Errorf("host: %v", resp["error"])
		}
		return resp, nil
	case <-time.After(5 * time.Second):
		lc.cancel(id)
		return nil, errors.New("request timed out")
	case <-lc.closed:
		lc.cancel(id)
		return nil, errors.New("connection closed")
	}
}

func (lc *liveClient) cancel(id int) {
	lc.pendMu.Lock()
	delete(lc.pending, id)
	lc.pendMu.Unlock()
}

// Events returns the channel of server-pushed param_changed notifications (buffered; drop-if-full). A controller
// can ignore events whose "by" equals its own clientID (its own echo).
func (lc *liveClient) Events() <-chan map[string]any { return lc.events }

// DrainEvents non-blockingly pulls every buffered server-pushed event (param_changed / governed_changed), oldest
// first, and returns them. It never blocks: it stops at the first empty read, so it is safe to poll.
func (lc *liveClient) DrainEvents() []map[string]any {
	var out []map[string]any
	for {
		select {
		case ev := <-lc.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// ClientID is the controller identity the host assigned at the ping handshake (0 if not connected).
func (lc *liveClient) ClientID() int { return lc.clientID }

// parseGoverned decodes the wire `governed` object into a GovernedState.
func parseGoverned(v any) GovernedState {
	g := GovernedState{SectionLeases: map[string]int{}}
	m, _ := v.(map[string]any)
	if m == nil {
		return g
	}
	if f, ok := m["instance_lease"].(float64); ok {
		g.InstanceLease = int(f)
	}
	if f, ok := m["generation"].(float64); ok {
		g.Generation = int(f)
	}
	if secs, ok := m["section_leases"].(map[string]any); ok {
		for name, hv := range secs {
			if hf, ok := hv.(float64); ok {
				g.SectionLeases[name] = int(hf)
			}
		}
	}
	return g
}

// Govern applies a governed command over the socket (the host runs the conflict tier on its single applier and
// pushes a governed_changed event). A rejection is a normal reply (resolution "rejected"), not an error.
func (lc *liveClient) Govern(op, group string) (string, GovernedState, error) {
	req := map[string]any{"cmd": "govern", "op": op}
	if group != "" {
		req["group"] = group
	}
	resp, err := lc.request(req)
	if err != nil {
		return "", GovernedState{}, err
	}
	res, _ := resp["resolution"].(string)
	return res, parseGoverned(resp["governed"]), nil
}

// GetGoverned reads the current governed state and the leasable sections (the plugin's param groups).
func (lc *liveClient) GetGoverned() (GovernedState, []string, error) {
	resp, err := lc.request(map[string]any{"cmd": "get_governed"})
	if err != nil {
		return GovernedState{}, nil, err
	}
	var sections []string
	if arr, ok := resp["sections"].([]any); ok {
		for _, s := range arr {
			if str, ok := s.(string); ok {
				sections = append(sections, str)
			}
		}
	}
	return parseGoverned(resp["governed"]), sections, nil
}

// ---- LiveEndpoint impl ----

func (lc *liveClient) SetParam(id string, v float64, isReal bool) (value, applied float64, text string, err error) {
	req := map[string]any{"cmd": "set_param", "param": id}
	if isReal {
		req["value"] = v // real units; the listener converts via the plugin's own (skew-aware) range
	} else {
		req["normalized"] = v
	}
	resp, err := lc.request(req)
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

// GetFullState / LoadState carry the plugin's whole patch as an OPAQUE base64 blob (base64 of the plugin's own
// getStateInformation bytes). The Go layer never decodes it - it round-trips the string - so it makes no
// assumption about the plugin's state format (XML-wrapped or raw binary both work).
func (lc *liveClient) GetFullState() (string, error) {
	resp, err := lc.request(map[string]any{"cmd": "get_full_state"})
	if err != nil {
		return "", err
	}
	state, _ := resp["state"].(string)
	return state, nil
}

func (lc *liveClient) LoadState(state string) error {
	_, err := lc.request(map[string]any{"cmd": "load_state", "state": state})
	return err
}

// SampleText sweeps a param across the given normalized points, collecting the plugin's rendered value text at
// each, then restores the param's original value. This is the Phase-1 probe primitive: the returned samples
// feed inferParam to recover the param's real unit/range/curve. It perturbs the live param transiently (and via
// the normal set path, so a DAW would see it); a production probe would snapshot/restore without notifying the
// host. Points default to 0, .1, .25, .5, .75, .9, 1 when nil.
func (lc *liveClient) SampleText(id string, points []float64) ([]ValueSample, error) {
	if points == nil {
		// 21 uniform points: dense enough for a good curve read and a close piecewise-linear seed; the exact
		// hit comes from binary-search refinement on top of this. One-time cost per param (cached).
		points = make([]float64, 0, 21)
		for i := 0; i <= 20; i++ {
			points = append(points, float64(i)/20.0)
		}
	}
	_, orig, _, err := lc.GetParam(id)
	if err != nil {
		return nil, err
	}
	out := make([]ValueSample, 0, len(points))
	for _, n := range points {
		_, _, text, err := lc.SetParam(id, n, false)
		if err != nil {
			return nil, err
		}
		out = append(out, ValueSample{Norm: n, Text: text})
	}
	_, _, _, _ = lc.SetParam(id, orig, false) // best-effort restore
	return out, nil
}

func (lc *liveClient) Close() {
	if lc == nil {
		return
	}
	lc.closeOnce.Do(func() {
		close(lc.closed) // wakes any waiting requests
		if lc.conn != nil {
			lc.conn.Close() // unblocks the reader goroutine's ReadBytes
		}
	})
}

// ---- live tools ----

type connectLiveIn struct {
	Host string `json:"host,omitempty" jsonschema:"host running the ControlServer (default 127.0.0.1; the server only binds loopback)"`
	Port int    `json:"port,omitempty" jsonschema:"listen port (default 51703; matches SIDECHAIN_PORT and cpp/ControlServer kDefaultPort)"`
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
	s.infer = map[string]ParamInference{} // new instance => any probed real-unit inferences are stale
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

// ---- full-state verbs (opaque save / recall / reset of the whole patch) ----

type loadStateIn struct {
	State string `json:"state" jsonschema:"an opaque plugin-state blob as returned by save_state. Round-tripped verbatim through the plugin's own setStateInformation; the bridge never inspects it."`
}

// handleSaveState pulls the running plugin's ENTIRE patch as one opaque blob (the plugin's own state XML). Use
// it to snapshot a patch you can restore later with load_state. Live only.
func (s *session) handleSaveState(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("save_state: not live. Call connect_live first."), nil, nil
	}
	xml, err := lc.GetFullState()
	if err != nil {
		return textResult("save_state failed: " + err.Error()), nil, nil
	}
	out := struct {
		State string `json:"state"`
	}{State: xml}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Saved live state (%d bytes). Pass it back to load_state to recall this patch.", len(xml))}}}, out, nil
}

// handleLoadState pushes a WHOLE patch (a blob from save_state) into the running instance. Live only.
func (s *session) handleLoadState(ctx context.Context, _ *mcp.CallToolRequest, in loadStateIn) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.State) == "" {
		return textResult("load_state: provide state (a blob from save_state)."), nil, nil
	}
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("load_state: not live. Call connect_live first."), nil, nil
	}
	if err := lc.LoadState(in.State); err != nil {
		return textResult("load_state failed: " + err.Error()), nil, nil
	}
	return textResult("Loaded state into the live instance (whole patch recalled)."), nil, nil
}

// handleResetInit recalls the host-supplied init/default patch on the running instance (a no-op ack if the host
// wired no reset hook). Live only.
func (s *session) handleResetInit(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("reset_init: not live. Call connect_live first."), nil, nil
	}
	if err := lc.ResetInit(); err != nil {
		return textResult("reset_init failed: " + err.Error()), nil, nil
	}
	return textResult("Reset the live instance to its init/default patch."), nil, nil
}

// registerLiveTools wires the live verbs. set_param/get_param forwarding is inside their handlers (they check
// s.live), so only the live-specific verbs are registered here.
func registerLiveTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "connect_live", Description: "Connect to a RUNNING Sidechain host (localhost) that has a plugin loaded, so set_param/get_param drive the live instance and play_note plays it."}, s.handleConnectLive)
	mcp.AddTool(srv, &mcp.Tool{Name: "disconnect_live", Description: "Disconnect from the live instance."}, s.handleDisconnectLive)
	mcp.AddTool(srv, &mcp.Tool{Name: "play_note", Description: "Play a MIDI note on the live instance (note, velocity, channel; optional holdMs to auto-release). Live only."}, s.handlePlayNote)
	mcp.AddTool(srv, &mcp.Tool{Name: "all_notes_off", Description: "Release all notes on the live instance (panic). Live only."}, s.handleAllNotesOff)
	mcp.AddTool(srv, &mcp.Tool{Name: "save_state", Description: "Snapshot the running plugin's ENTIRE patch as one opaque blob (the plugin's own state). Pass it back to load_state to recall the patch. Live only."}, s.handleSaveState)
	mcp.AddTool(srv, &mcp.Tool{Name: "load_state", Description: "Recall a WHOLE patch on the running plugin from a blob returned by save_state. Live only."}, s.handleLoadState)
	mcp.AddTool(srv, &mcp.Tool{Name: "reset_init", Description: "Reset the running plugin to its init/default patch (host-supplied). Live only."}, s.handleResetInit)
}
