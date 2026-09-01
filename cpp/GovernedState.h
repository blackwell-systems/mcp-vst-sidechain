#pragma once
#include <cstdint>
#include <iterator>
#include <map>
#include <string>

// ================================================================================================
// sidechain::GovState - the C3 governed coordination state (see docs/CONCURRENCY.md). This is the runtime port of
// the Go reference model in governed.go; governed_test.go is the EXECUTABLE PROOF (an exhaustive enumeration) that
// these invariants hold. The two share one algebra - keep any change to reduce/ok/repair/apply mirrored.
//
// Representation difference (intentional): the Go model keys section leases by a small fixed set of representative
// scope indices, so the state space is finite and enumerable; here they are keyed by the plugin's actual param
// GROUP NAMES (bound by ControlServer from PluginBridge::sectionGroups), so any real section is leasable. The
// algebra is identical and per-scope-INDEPENDENT (each section interacts only with the instance lease, never with
// another section - governed_test.go's TestGovSectionsIndependent makes this executable), so the enumeration over
// a few representative scopes covers any set of named sections.
//
// It models the REAL coordination concerns of several controllers driving ONE plugin instance, kept separate from
// the continuous plugin params (which stay last-writer-wins):
//   - Hierarchical edit leases: a whole-instance lease and per-section leases. If one controller holds the whole
//     instance, no OTHER controller may hold a section of it - taking the instance revokes others' section leases
//     (the compensate path), and taking a section of an instance held by another is refused (a reject guard).
//   - Patch generation: a monotone counter bumped on a whole-patch change (load_state / reset_init).
//   - Disconnect cleanup: a departing/crashed controller's leases are all released (ControllerGone).
//
// A lease holds an arbitrary controller id; the map holds only ACTIVE section leases (an absent group == free).
// ================================================================================================

namespace sidechain
{

enum class Resolution : uint8_t { Applied, Compensated, Rejected };

// The governed commands. AcquireInstance/ReleaseInstance/AcquireSection/ReleaseSection are controller-facing;
// BumpGeneration and ControllerGone are raised internally by the server (a whole-patch change, a disconnect).
enum class GovOp : uint8_t { AcquireInstance, ReleaseInstance, AcquireSection, ReleaseSection, BumpGeneration, ControllerGone };

inline constexpr int kMinGeneration = 0;
inline constexpr int kMaxGeneration = 2;   // generation saturates so the enumerated model stays finite

// One governed command. Only the fields relevant to its op are read.
struct GovCmd
{
    GovOp       op = GovOp::BumpGeneration;
    int         by = 0;        // originating / departing controller
    std::string section;       // AcquireSection / ReleaseSection: the param-group name
};

struct GovState
{
    int                        soloInstanceLease = 0;   // controller holding the whole-instance edit lease; 0 = none
    std::map<std::string, int> sectionLease;            // group name -> holder id (only ACTIVE leases; absent = free)
    int                        generation = 0;          // kMinGeneration..kMaxGeneration

    bool operator== (const GovState& o) const noexcept
    {
        return soloInstanceLease == o.soloInstanceLease && generation == o.generation && sectionLease == o.sectionLease;
    }

    // The invariant predicate every reachable state must satisfy.
    bool ok() const noexcept
    {
        if (generation < kMinGeneration || generation > kMaxGeneration) return false;
        if (soloInstanceLease != 0)
            for (const auto& kv : sectionLease)
                if (kv.second != soloInstanceLease)
                    return false; // hierarchy: a held instance forbids another controller's section lease
        return true;
    }

    // A deterministic, TOTAL map into the nearest legal state (the compensation): taking the whole instance
    // revokes any section held by another controller.
    GovState repair() const
    {
        GovState s = *this;
        if (s.generation < kMinGeneration) s.generation = kMinGeneration;
        if (s.generation > kMaxGeneration) s.generation = kMaxGeneration;
        if (s.soloInstanceLease != 0)
            for (auto it = s.sectionLease.begin(); it != s.sectionLease.end(); )
                it = (it->second != s.soloInstanceLease) ? s.sectionLease.erase (it) : std::next (it);
        return s;
    }

    // The raw next state for a command (pure; the result may be illegal).
    GovState reduce (const GovCmd& c) const
    {
        GovState s = *this;
        switch (c.op)
        {
            case GovOp::AcquireInstance: s.soloInstanceLease = c.by; break;
            case GovOp::ReleaseInstance: if (s.soloInstanceLease == c.by) s.soloInstanceLease = 0; break;
            case GovOp::AcquireSection:  s.sectionLease[c.section] = c.by; break;
            case GovOp::ReleaseSection:
                { auto it = s.sectionLease.find (c.section); if (it != s.sectionLease.end() && it->second == c.by) s.sectionLease.erase (it); }
                break;
            case GovOp::BumpGeneration:  if (s.generation < kMaxGeneration) ++s.generation; break;
            case GovOp::ControllerGone:
                if (s.soloInstanceLease == c.by) s.soloInstanceLease = 0;
                for (auto it = s.sectionLease.begin(); it != s.sectionLease.end(); )
                    it = (it->second == c.by) ? s.sectionLease.erase (it) : std::next (it);
                break;
        }
        return s;
    }

    // The conflict tier: returns the next state and how the command resolved. Run on the single applier thread, so
    // the check-then-commit is atomic (no TOCTOU race). Lease guards reject; the only residual illegality
    // (AcquireInstance while another holds a section) is compensated by repair().
    GovState apply (const GovCmd& c, Resolution& res) const
    {
        switch (c.op)
        {
            case GovOp::AcquireInstance:
                if (soloInstanceLease != 0 && soloInstanceLease != c.by) { res = Resolution::Rejected; return *this; }
                break;
            case GovOp::AcquireSection:
            {
                if (soloInstanceLease != 0 && soloInstanceLease != c.by) { res = Resolution::Rejected; return *this; }
                auto it = sectionLease.find (c.section);
                if (it != sectionLease.end() && it->second != c.by)      { res = Resolution::Rejected; return *this; }
                break;
            }
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
