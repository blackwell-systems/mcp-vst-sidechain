#pragma once
#include <cstdint>

// ================================================================================================
// sidechain::GovState - the C3 governed coordination state (see docs/CONCURRENCY.md). This is the C++ port of the
// Go reference model in governed.go; governed_test.go is the EXECUTABLE PROOF (an exhaustive enumeration over every
// reachable state) that these invariants hold. Keep the two in lockstep: any change to reduce/ok/repair/apply here
// must be mirrored there so the enumeration still covers it.
//
// It models the REAL coordination concerns of several controllers driving ONE plugin instance, kept separate from
// the continuous plugin params (which stay last-writer-wins):
//   - Hierarchical edit leases: a whole-instance lease and per-section leases. If one controller holds the whole
//     instance, no OTHER controller may hold a section of it - taking the instance revokes others' section leases
//     (the compensate path), and taking a section of an instance held by another is refused (a reject guard).
//   - Patch generation: a monotone counter bumped on a whole-patch change (load_state / reset_init), so an agent
//     can detect that the base it was editing moved under it.
//   - Disconnect cleanup: a departing/crashed controller's leases are all released (ControllerGone), the invariant
//     that makes leases safe.
//
// A lease field holds an arbitrary controller id (0 = free); its numeric value is NOT an invariant. The section
// scope is an edit region indexed 0..kScopeCount-1 (a deployment maps these to the plugin's param groups).
// ================================================================================================

namespace sidechain
{

enum class Resolution : uint8_t { Applied, Compensated, Rejected };

// The governed commands. AcquireInstance/ReleaseInstance/AcquireSection/ReleaseSection are controller-facing;
// BumpGeneration and ControllerGone are raised internally by the server (a whole-patch change, a disconnect).
enum class GovOp : uint8_t { AcquireInstance, ReleaseInstance, AcquireSection, ReleaseSection, BumpGeneration, ControllerGone };

inline constexpr int kScopeCount    = 3;
inline constexpr int kMinGeneration = 0;
inline constexpr int kMaxGeneration = 2;   // generation saturates so the enumerated space stays finite

// One governed command. Only the fields relevant to its op are read.
struct GovCmd
{
    GovOp op    = GovOp::BumpGeneration;
    int   by    = 0;   // originating / departing controller
    int   scope = 0;   // AcquireSection / ReleaseSection
};

struct GovState
{
    int soloInstanceLease = 0;                  // controller holding the whole-instance edit lease; 0 = none
    int sectionLease[kScopeCount] = { 0 };      // controller holding each section's edit lease; 0 = none
    int generation = 0;                         // kMinGeneration..kMaxGeneration

    bool operator== (const GovState& o) const noexcept
    {
        if (soloInstanceLease != o.soloInstanceLease || generation != o.generation) return false;
        for (int i = 0; i < kScopeCount; ++i)
            if (sectionLease[i] != o.sectionLease[i]) return false;
        return true;
    }

    // The invariant predicate every reachable state must satisfy.
    bool ok() const noexcept
    {
        if (generation < kMinGeneration || generation > kMaxGeneration) return false;
        for (int i = 0; i < kScopeCount; ++i)
            if (soloInstanceLease != 0 && sectionLease[i] != 0 && sectionLease[i] != soloInstanceLease)
                return false; // hierarchy: a held instance forbids another controller's section lease
        return true;
    }

    // A deterministic, TOTAL map into the nearest legal state (the compensation): taking the whole instance
    // revokes any section held by another controller.
    GovState repair() const noexcept
    {
        GovState s = *this;
        if (s.generation < kMinGeneration) s.generation = kMinGeneration;
        if (s.generation > kMaxGeneration) s.generation = kMaxGeneration;
        if (s.soloInstanceLease != 0)
            for (int i = 0; i < kScopeCount; ++i)
                if (s.sectionLease[i] != 0 && s.sectionLease[i] != s.soloInstanceLease)
                    s.sectionLease[i] = 0;
        return s;
    }

    // The raw next state for a command (pure; the result may be illegal).
    GovState reduce (const GovCmd& c) const noexcept
    {
        GovState s = *this;
        switch (c.op)
        {
            case GovOp::AcquireInstance: s.soloInstanceLease = c.by; break;
            case GovOp::ReleaseInstance: if (s.soloInstanceLease == c.by) s.soloInstanceLease = 0; break;
            case GovOp::AcquireSection:  if (c.scope >= 0 && c.scope < kScopeCount) s.sectionLease[c.scope] = c.by; break;
            case GovOp::ReleaseSection:  if (c.scope >= 0 && c.scope < kScopeCount && s.sectionLease[c.scope] == c.by) s.sectionLease[c.scope] = 0; break;
            case GovOp::BumpGeneration:  if (s.generation < kMaxGeneration) ++s.generation; break;
            case GovOp::ControllerGone:
                if (s.soloInstanceLease == c.by) s.soloInstanceLease = 0;
                for (int i = 0; i < kScopeCount; ++i)
                    if (s.sectionLease[i] == c.by) s.sectionLease[i] = 0;
                break;
        }
        return s;
    }

    // The conflict tier: returns the next state and how the command resolved. Run on the single applier thread, so
    // the check-then-commit is atomic (no TOCTOU race). Lease guards reject; the only residual illegality
    // (AcquireInstance while another holds a section) is compensated by repair().
    GovState apply (const GovCmd& c, Resolution& res) const noexcept
    {
        switch (c.op)
        {
            case GovOp::AcquireInstance:
                if (soloInstanceLease != 0 && soloInstanceLease != c.by) { res = Resolution::Rejected; return *this; }
                break;
            case GovOp::AcquireSection:
                if (c.scope < 0 || c.scope >= kScopeCount)                 { res = Resolution::Rejected; return *this; }
                if (soloInstanceLease != 0 && soloInstanceLease != c.by)   { res = Resolution::Rejected; return *this; }
                if (sectionLease[c.scope] != 0 && sectionLease[c.scope] != c.by) { res = Resolution::Rejected; return *this; }
                break;
            case GovOp::ReleaseInstance:
            case GovOp::ReleaseSection:
            case GovOp::BumpGeneration:
            case GovOp::ControllerGone:
                break;   // no guard: reduce cannot produce an illegal state for these
        }
        const GovState n = reduce (c);
        if (n.ok()) { res = Resolution::Applied; return n; }
        res = Resolution::Compensated;
        return n.repair();
    }
};

} // namespace sidechain
