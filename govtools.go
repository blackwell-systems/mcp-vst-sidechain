// govtools.go - the MCP tool surface for the concurrency layer (C2 change events + C3 governed coordination). It
// exposes to an agent what the ControlServer already speaks on the wire: claim/release an exclusive edit lease on
// the whole instance or a param-group section (so several agents driving one plugin do not fight over the same
// knobs), read the current leases, and poll the changes other controllers made. Live only; the governed state
// lives on the running host.

package sidechain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type acquireLeaseIn struct {
	Section string `json:"section,omitempty" jsonschema:"the param group to lease exclusively (from list_params / get_leases). Omit to lease the WHOLE instance, which also revokes others' section leases."`
}

type releaseLeaseIn struct {
	Section string `json:"section,omitempty" jsonschema:"the param group whose lease to release. Omit to release the whole-instance lease."`
}

type pollEventsIn struct {
	IncludeSelf bool `json:"includeSelf,omitempty" jsonschema:"include changes you caused yourself (echoes); default false shows only what OTHER controllers changed."`
}

// leaseResult is the structured payload for the lease tools: how the request resolved (for acquire/release) plus
// the current governed state and this controller's own id.
type leaseResult struct {
	Resolution string         `json:"resolution,omitempty"` // applied / compensated / rejected (acquire/release only)
	You        int            `json:"you"`                  // this controller's client id
	Instance   int            `json:"instance_lease"`       // holder of the whole-instance lease; 0 = free
	Sections   map[string]int `json:"section_leases"`       // param group -> holder; only held sections
	Generation int            `json:"generation"`           // patch generation (bumped on a whole-patch change)
	Leasable   []string       `json:"leasable,omitempty"`   // the param groups a section lease can be taken on
}

func govSummary(you int, g GovernedState) string {
	var b strings.Builder
	if g.InstanceLease == 0 {
		b.WriteString("instance lease: free")
	} else if g.InstanceLease == you {
		b.WriteString("instance lease: you")
	} else {
		fmt.Fprintf(&b, "instance lease: client %d", g.InstanceLease)
	}
	if len(g.SectionLeases) > 0 {
		names := make([]string, 0, len(g.SectionLeases))
		for n := range g.SectionLeases {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			who := fmt.Sprintf("client %d", g.SectionLeases[n])
			if g.SectionLeases[n] == you {
				who = "you"
			}
			parts = append(parts, fmt.Sprintf("%q->%s", n, who))
		}
		fmt.Fprintf(&b, "; sections: %s", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "; generation %d", g.Generation)
	return b.String()
}

func (s *session) governedEndpoint() LiveEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

func (s *session) handleAcquireLease(ctx context.Context, _ *mcp.CallToolRequest, in acquireLeaseIn) (*mcp.CallToolResult, any, error) {
	lc := s.governedEndpoint()
	if lc == nil {
		return textResult("acquire_lease: not live. Call connect_live first."), nil, nil
	}
	op, scope := "acquire_instance", "the whole instance"
	if strings.TrimSpace(in.Section) != "" {
		op, scope = "acquire_section", fmt.Sprintf("section %q", in.Section)
	}
	res, gov, err := lc.Govern(op, in.Section)
	if err != nil {
		return textResult(fmt.Sprintf("acquire_lease %s failed: %v", scope, err)), nil, nil
	}
	you := lc.ClientID()
	var msg string
	switch res {
	case "applied":
		msg = fmt.Sprintf("Acquired %s (you are client %d).", scope, you)
	case "compensated":
		msg = fmt.Sprintf("Acquired %s (you are client %d); conflicting section leases held by others were revoked.", scope, you)
	case "rejected":
		msg = fmt.Sprintf("Denied %s: it is held by another controller. Retry after it is released.", scope)
	default:
		msg = fmt.Sprintf("acquire_lease %s -> %s.", scope, res)
	}
	out := leaseResult{Resolution: res, You: you, Instance: gov.InstanceLease, Sections: gov.SectionLeases, Generation: gov.Generation}
	return textResult(msg + " Now: " + govSummary(you, gov)), out, nil
}

func (s *session) handleReleaseLease(ctx context.Context, _ *mcp.CallToolRequest, in releaseLeaseIn) (*mcp.CallToolResult, any, error) {
	lc := s.governedEndpoint()
	if lc == nil {
		return textResult("release_lease: not live. Call connect_live first."), nil, nil
	}
	op, scope := "release_instance", "the whole instance"
	if strings.TrimSpace(in.Section) != "" {
		op, scope = "release_section", fmt.Sprintf("section %q", in.Section)
	}
	res, gov, err := lc.Govern(op, in.Section)
	if err != nil {
		return textResult(fmt.Sprintf("release_lease %s failed: %v", scope, err)), nil, nil
	}
	you := lc.ClientID()
	out := leaseResult{Resolution: res, You: you, Instance: gov.InstanceLease, Sections: gov.SectionLeases, Generation: gov.Generation}
	return textResult(fmt.Sprintf("Released %s. Now: %s", scope, govSummary(you, gov))), out, nil
}

func (s *session) handleGetLeases(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	lc := s.governedEndpoint()
	if lc == nil {
		return textResult("get_leases: not live. Call connect_live first."), nil, nil
	}
	gov, sections, err := lc.GetGoverned()
	if err != nil {
		return textResult("get_leases failed: " + err.Error()), nil, nil
	}
	you := lc.ClientID()
	out := leaseResult{You: you, Instance: gov.InstanceLease, Sections: gov.SectionLeases, Generation: gov.Generation, Leasable: sections}
	msg := fmt.Sprintf("You are client %d. %s.", you, govSummary(you, gov))
	if len(sections) > 0 {
		msg += " Leasable sections: " + strings.Join(sections, ", ") + "."
	}
	return textResult(msg), out, nil
}

type paramChange struct {
	Param      string  `json:"param"`
	Value      float64 `json:"value"`
	Normalized float64 `json:"normalized"`
	Text       string  `json:"text"`
	By         int     `json:"by"`
}

type pollResult struct {
	ParamChanges []paramChange  `json:"param_changes,omitempty"`
	Governed     *GovernedState `json:"governed,omitempty"`
}

func (s *session) handlePollEvents(ctx context.Context, _ *mcp.CallToolRequest, in pollEventsIn) (*mcp.CallToolResult, any, error) {
	lc := s.governedEndpoint()
	if lc == nil {
		return textResult("poll_events: not live. Call connect_live first."), nil, nil
	}
	you := lc.ClientID()
	raw := lc.DrainEvents()

	// Collapse to the latest state per param, keep the latest governed snapshot, drop our own echoes by default.
	latest := map[string]paramChange{}
	var order []string
	var gov *GovernedState
	for _, ev := range raw {
		by := 0
		if f, ok := ev["by"].(float64); ok {
			by = int(f)
		}
		if !in.IncludeSelf && by == you {
			continue
		}
		switch ev["event"] {
		case "param_changed":
			p, _ := ev["param"].(string)
			pc := paramChange{Param: p, By: by}
			pc.Value, _ = ev["value"].(float64)
			pc.Normalized, _ = ev["normalized"].(float64)
			pc.Text, _ = ev["text"].(string)
			if _, seen := latest[p]; !seen {
				order = append(order, p)
			}
			latest[p] = pc
		case "governed_changed":
			g := parseGoverned(ev["governed"])
			gov = &g
		}
	}

	changes := make([]paramChange, 0, len(order))
	for _, p := range order {
		changes = append(changes, latest[p])
	}
	if len(changes) == 0 && gov == nil {
		return textResult("No changes from other controllers since the last poll."), pollResult{}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d param change(s) from other controllers since the last poll", len(changes))
	if gov != nil {
		fmt.Fprintf(&b, " (+ a governed change: %s)", govSummary(you, *gov))
	}
	b.WriteString(".")
	for i, c := range changes {
		if i >= 12 {
			fmt.Fprintf(&b, " ... and %d more.", len(changes)-12)
			break
		}
		fmt.Fprintf(&b, " %s=%s (by %d);", c.Param, c.Text, c.By)
	}
	return textResult(b.String()), pollResult{ParamChanges: changes, Governed: gov}, nil
}

// registerGovernedTools wires the concurrency tools (C2/C3). Called alongside registerLiveTools.
func registerGovernedTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "acquire_lease", Description: "Claim an EXCLUSIVE edit lease on a param-group section (pass section=), or the whole instance (omit section), so other agents driving the same plugin are refused edits there. Returns applied/compensated/rejected. Live only."}, s.handleAcquireLease)
	mcp.AddTool(srv, &mcp.Tool{Name: "release_lease", Description: "Release an edit lease you hold (a section, or the whole instance). Live only."}, s.handleReleaseLease)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_leases", Description: "Show the current edit leases (who holds the instance and each section), the leasable sections, the patch generation, and your own client id. Live only."}, s.handleGetLeases)
	mcp.AddTool(srv, &mcp.Tool{Name: "poll_events", Description: "Return what changed on the plugin since your last poll: parameter changes and governed (lease/generation) changes made by OTHER controllers (or yourself with includeSelf=true). Live only."}, s.handlePollEvents)
}
