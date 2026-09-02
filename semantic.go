// semantic.go - the Phase 3 persistent semantic store (see docs/PHASE3-SCOPING.md). It persists, per plugin,
// everything the bridge and the agent learn about a plugin's parameters, so probing is paid once EVER (not per
// session) and the agent's own semantics (role, aliases, polarity) survive restarts and accumulate.
//
// Storage is a DIRECTORY of per-fingerprint files (one per plugin surface), never one shared file: writes are
// atomic (temp + rename) and merge-on-write (re-read, field-level union), so two processes on different plugins
// never contend and two on the same plugin at worst lose a same-field edit, never wholesale-clobber. A parameter
// is classified on two orthogonal axes: a DERIVED behavior class (a signature from the Phase-1 inference, e.g.
// "float:log:hz") and an agent-authored, free-form ROLE (e.g. "filter.cutoff"); the bridge enforces no ontology.

package sidechain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// persistableInference is the on-disk form of a ParamInference: the exported semantics PLUS the runtime inversion
// data (dense norm->real samples and discrete label positions), so a persisted inference is fully usable next
// session (analytic AND sampled set_param real=, and choice= on a discrete-as-float param) without re-probing.
// Kept separate from ParamInference's own JSON so describe_param's wire shape is unaffected.
type persistableInference struct {
	Numeric       bool               `json:"numeric"`
	Unit          string             `json:"unit,omitempty"`
	RealMin       float64            `json:"realMin,omitempty"`
	RealMax       float64            `json:"realMax,omitempty"`
	Bipolar       bool               `json:"bipolar,omitempty"`
	Curve         string             `json:"curve,omitempty"`
	Labels        []string           `json:"labels,omitempty"`
	Fit           *CurveFit          `json:"fit,omitempty"`
	Samples       [][2]float64       `json:"samples,omitempty"` // [norm, real] pairs (the inversion table)
	DiscreteNorms map[string]float64 `json:"discreteNorms,omitempty"`
}

// toPersistable / toInference convert a ParamInference to and from its stored form. Same-package access to the
// unexported runtime fields (table, discreteNorms) is why these live here rather than on the type.
func toPersistable(pi ParamInference) persistableInference {
	p := persistableInference{
		Numeric: pi.Numeric, Unit: pi.Unit, RealMin: pi.RealMin, RealMax: pi.RealMax,
		Bipolar: pi.Bipolar, Curve: pi.Curve, Labels: pi.Labels, Fit: pi.Fit, DiscreteNorms: pi.discreteNorms,
	}
	for _, s := range pi.table {
		p.Samples = append(p.Samples, [2]float64{s.norm, s.real})
	}
	return p
}

func (p persistableInference) toInference() ParamInference {
	pi := ParamInference{
		Numeric: p.Numeric, Unit: p.Unit, RealMin: p.RealMin, RealMax: p.RealMax,
		Bipolar: p.Bipolar, Curve: p.Curve, Labels: p.Labels, Fit: p.Fit, discreteNorms: p.DiscreteNorms,
	}
	for _, s := range p.Samples {
		pi.table = append(pi.table, realSample{s[0], s[1]})
	}
	return pi
}

// ParamSemantics is what the store persists per parameter. inference + behaviorClass are auto-derived on probe;
// everything from role down is agent-authored and merge-updated (only provided fields change).
type ParamSemantics struct {
	Label         string                `json:"label,omitempty"`
	Inference     *persistableInference `json:"inference,omitempty"`     // nil until probed
	BehaviorClass string                `json:"behaviorClass,omitempty"` // derived signature (axis 1), e.g. "float:log:hz"
	Role          string                `json:"role,omitempty"`          // agent-authored (axis 2), e.g. "filter.cutoff"
	Aliases       []string              `json:"aliases,omitempty"`
	Polarity      string                `json:"polarity,omitempty"`
	Section       string                `json:"section,omitempty"`
	Confidence    float64               `json:"confidence,omitempty"`
	Notes         string                `json:"notes,omitempty"`
}

// SemanticEntry is one plugin's stored semantics: its identity plus per-param semantics, keyed by param id.
type SemanticEntry struct {
	Fingerprint string                     `json:"fingerprint"`
	Plugin      PluginIdentity             `json:"plugin"`
	Params      map[string]*ParamSemantics `json:"params"`
}

func (e *SemanticEntry) param(id string) *ParamSemantics {
	if e.Params == nil {
		e.Params = map[string]*ParamSemantics{}
	}
	if e.Params[id] == nil {
		e.Params[id] = &ParamSemantics{}
	}
	return e.Params[id]
}

// fingerprintCatalog is the store key for a catalog: a stable hash over the plugin surface. Version is deliberately
// NOT included, so a surface-preserving version bump reuses the cached semantics; a bump that adds/removes/renames
// params changes the sorted ids or count and so the fingerprint, yielding a fresh (non-destructive) entry.
func fingerprintCatalog(cat ParamCatalog) string {
	all := cat.All()
	ids := make([]string, len(all))
	for i, p := range all {
		ids[i] = p.ID
	}
	sort.Strings(ids)
	id := identityOf(cat)
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%d", id.Name, id.Manufacturer, id.Format, strings.Join(ids, ","), len(all))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// identityOf returns the plugin identity of a catalog (zero value for a custom ParamCatalog without one).
func identityOf(cat ParamCatalog) PluginIdentity {
	if c, ok := cat.(*Catalog); ok {
		return c.Plugin
	}
	return PluginIdentity{}
}

// behaviorClass derives the axis-1 signature from a Phase-1 inference: type:curve:unit[:bipolar] for a numeric
// param, "discrete:enum" for a labelled discrete one, "opaque" when there is no readable value text. It is a
// normalized STRING (not a fixed enum), so a manipulation can be generic over all params of a class.
func behaviorClass(pi ParamInference) string {
	if !pi.Numeric {
		if len(pi.Labels) > 0 {
			return "discrete:enum"
		}
		return "opaque"
	}
	unit := pi.Unit
	if unit == "" {
		unit = "unitless"
	}
	sig := "float:" + curveTag(pi.Curve) + ":" + unit
	if pi.Bipolar {
		sig += ":bipolar"
	}
	return sig
}

func curveTag(curve string) string {
	switch curve {
	case "logarithmic":
		return "log"
	case "exponential":
		return "exp"
	case "linear":
		return "linear"
	case "flat":
		return "flat"
	default:
		return "unknown"
	}
}

// SemanticStore is a directory of per-fingerprint JSON files. A process mutex serializes this process's writes;
// cross-process safety comes from the per-fingerprint layout + atomic temp/rename + merge-on-write.
type SemanticStore struct {
	dir string
	mu  sync.Mutex
}

// NewSemanticStore returns a store rooted at dir (created lazily on first write).
func NewSemanticStore(dir string) *SemanticStore { return &SemanticStore{dir: dir} }

// defaultSemanticDir is the per-user cache location, so the store persists and is reused regardless of the working
// directory. SIDECHAIN_SEMANTIC_DIR overrides it.
func defaultSemanticDir() string {
	if d := strings.TrimSpace(os.Getenv("SIDECHAIN_SEMANTIC_DIR")); d != "" {
		return d
	}
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "sidechain")
	}
	return filepath.Join(os.TempDir(), "sidechain")
}

func (st *SemanticStore) path(fingerprint string) string {
	return filepath.Join(st.dir, strings.TrimPrefix(fingerprint, "sha256:")+".json")
}

// Load reads the entry for a fingerprint, or returns (nil, nil) if there is none yet.
func (st *SemanticStore) Load(fingerprint string) (*SemanticEntry, error) {
	data, err := os.ReadFile(st.path(fingerprint))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e SemanticEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("parse semantic store %s: %w", st.path(fingerprint), err)
	}
	if e.Params == nil {
		e.Params = map[string]*ParamSemantics{}
	}
	return &e, nil
}

// Save merges the entry into any on-disk entry (field-level union, this writer's non-empty fields win) and writes
// it atomically. It returns the merged entry so the caller can adopt it as its in-memory view.
func (st *SemanticStore) Save(e *SemanticEntry) (*SemanticEntry, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		return nil, err
	}
	disk, err := st.Load(e.Fingerprint)
	if err != nil {
		return nil, err
	}
	merged := mergeEntry(disk, e)
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(st.path(e.Fingerprint), data); err != nil {
		return nil, err
	}
	return merged, nil
}

// Forget removes a fingerprint's entry (no error if it does not exist).
func (st *SemanticStore) Forget(fingerprint string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	err := os.Remove(st.path(fingerprint))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// mergeEntry unions disk and ours: params only on disk are preserved; for shared params, each field is taken from
// ours when set, else from disk (so a concurrent process's annotation on a field we did not touch survives).
func mergeEntry(disk, ours *SemanticEntry) *SemanticEntry {
	if disk == nil {
		return ours
	}
	out := &SemanticEntry{Fingerprint: ours.Fingerprint, Plugin: ours.Plugin, Params: map[string]*ParamSemantics{}}
	for id, s := range disk.Params {
		cp := *s
		out.Params[id] = &cp
	}
	for id, s := range ours.Params {
		out.Params[id] = mergeParam(out.Params[id], s)
	}
	return out
}

func mergeParam(base, upd *ParamSemantics) *ParamSemantics {
	if base == nil {
		cp := *upd
		return &cp
	}
	r := *base
	if upd.Label != "" {
		r.Label = upd.Label
	}
	if upd.Inference != nil {
		r.Inference = upd.Inference
	}
	if upd.BehaviorClass != "" {
		r.BehaviorClass = upd.BehaviorClass
	}
	if upd.Role != "" {
		r.Role = upd.Role
	}
	if upd.Aliases != nil {
		r.Aliases = upd.Aliases
	}
	if upd.Polarity != "" {
		r.Polarity = upd.Polarity
	}
	if upd.Section != "" {
		r.Section = upd.Section
	}
	if upd.Confidence != 0 {
		r.Confidence = upd.Confidence
	}
	if upd.Notes != "" {
		r.Notes = upd.Notes
	}
	return &r
}

// ---- session integration ----

// attachStore binds a store to the session: it loads (or creates) this catalog's entry by fingerprint and
// hydrates the in-memory inference cache from any persisted inferences, so a param probed in a past session is
// already known. Caller must not hold s.mu.
func (s *session) attachStore(store *SemanticStore) error {
	fp := fingerprintCatalog(s.catalog)
	e, err := store.Load(fp)
	if err != nil {
		return err
	}
	if e == nil {
		e = &SemanticEntry{Fingerprint: fp, Plugin: identityOf(s.catalog), Params: map[string]*ParamSemantics{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
	s.entry = e
	s.hydrateInferLocked()
	return nil
}

// hydrateInferLocked repopulates s.infer from the store entry's persisted inferences. Caller holds s.mu.
func (s *session) hydrateInferLocked() {
	if s.entry == nil {
		return
	}
	s.infer = map[string]ParamInference{}
	for id, sem := range s.entry.Params {
		if sem.Inference != nil {
			s.infer[id] = sem.Inference.toInference()
		}
	}
}

// reloadSemanticsLocked re-reads the store entry from disk (picking up any external changes) and rehydrates the
// inference cache. Used on connect_live so reconnecting to the SAME plugin recalls persisted inferences instead of
// forcing a re-probe. Caller holds s.mu. Falls back to clearing infer when there is no store.
func (s *session) reloadSemanticsLocked() {
	if s.store == nil {
		s.infer = map[string]ParamInference{}
		return
	}
	if e, err := s.store.Load(s.entry.Fingerprint); err == nil && e != nil {
		s.entry = e
	}
	s.hydrateInferLocked()
}

// recordInferenceLocked persists a freshly-probed inference (and its derived behavior class) to the store,
// merging with disk. Caller holds s.mu. A store error is non-fatal (the in-memory cache still works).
func (s *session) recordInferenceLocked(id, label string, pi ParamInference) {
	if s.store == nil {
		return
	}
	sem := s.entry.param(id)
	if label != "" {
		sem.Label = label
	}
	p := toPersistable(pi)
	sem.Inference = &p
	sem.BehaviorClass = behaviorClass(pi)
	if merged, err := s.store.Save(s.entry); err == nil {
		s.entry = merged
	}
}

// writeAtomic writes data to a temp file in the target's directory and renames it over the target (atomic on the
// same filesystem), so a reader never sees a torn or half-written file even on a crash mid-write.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
