#pragma once
#include <cstdint>

// ================================================================================================
// sidechain::GovState - the C3 governed coordination state (see docs/CONCURRENCY.md). This is the C++ port of the
// Go reference model in governed.go; governed_test.go is the EXECUTABLE PROOF (an exhaustive enumeration over every
// reachable state) that these invariants hold. Keep the two in lockstep: any change to reduce/ok/repair/apply here
// must be mirrored there so the enumeration still covers it.
//
// This is the small, discrete, invariant-bearing state the conflict tier governs on the SINGLE applier thread (the
// message thread). It is deliberately NOT the continuous plugin parameters, which stay last-writer-wins: they carry
// no cross-parameter invariants and cannot be enumerated. The schema here (an exclusive-edit lease, a voice-mode
// gate, a panic/playback latch) is illustrative - enough to exercise a reject policy and a compensate policy - and
// is meant to be replaced by the real coordination schema once multi-controller use surfaces the actual conflicts.
//
// soloLease holds an arbitrary controller id (0 = free); its numeric value is NOT an invariant. Lease exclusivity
// is a transition guard in apply(), not a state predicate, so ok()/repair() never inspect the id's magnitude.
// ================================================================================================

namespace sidechain
{

enum class VoiceMode  : uint8_t { Poly, Mono, Legato };
enum class Resolution : uint8_t { Applied, Compensated, Rejected };
enum class GovOp      : uint8_t { AcquireSolo, ReleaseSolo, SetMode, SetBudget, SetPanic, SetPlaying };

inline constexpr int kMinVoiceBudget = 1;
inline constexpr int kMaxVoiceBudget = 4;

// One governed command. Only the fields relevant to its op are read.
struct GovCmd
{
    GovOp     op   = GovOp::SetPanic;
    int       by   = 0;                 // originating controller (lease ops)
    VoiceMode mode = VoiceMode::Poly;   // SetMode
    int       n    = 0;                 // SetBudget
    bool      on   = false;             // SetPanic / SetPlaying
};

struct GovState
{
    int       soloLease   = 0;                 // controller id holding the exclusive-edit lease; 0 = none
    VoiceMode mode        = VoiceMode::Poly;
    int       voiceBudget = 1;                 // kMinVoiceBudget..kMaxVoiceBudget
    bool      panicLatch  = false;
    bool      playing     = false;

    bool operator== (const GovState& o) const noexcept
    {
        return soloLease   == o.soloLease
            && mode        == o.mode
            && voiceBudget == o.voiceBudget
            && panicLatch  == o.panicLatch
            && playing     == o.playing;
    }

    // The invariant predicate every reachable state must satisfy.
    bool ok() const noexcept
    {
        if (voiceBudget < kMinVoiceBudget || voiceBudget > kMaxVoiceBudget) return false;
        if (mode != VoiceMode::Poly && voiceBudget != 1)                    return false; // single-voice mode => budget 1
        if (panicLatch && playing)                                          return false; // panic and playback exclusive
        return true;
    }

    // A deterministic, TOTAL map into the nearest legal state (the compensation).
    GovState repair() const noexcept
    {
        GovState s = *this;
        if (s.voiceBudget < kMinVoiceBudget) s.voiceBudget = kMinVoiceBudget;
        if (s.voiceBudget > kMaxVoiceBudget) s.voiceBudget = kMaxVoiceBudget;
        if (s.mode != VoiceMode::Poly)       s.voiceBudget = 1;      // single-voice mode clamps the budget
        if (s.panicLatch)                    s.playing = false;      // panic dominates playback
        return s;
    }

    // The raw next state for a command (pure; the result may be illegal).
    GovState reduce (const GovCmd& c) const noexcept
    {
        GovState s = *this;
        switch (c.op)
        {
            case GovOp::AcquireSolo: s.soloLease = c.by; break;
            case GovOp::ReleaseSolo: if (s.soloLease == c.by) s.soloLease = 0; break;
            case GovOp::SetMode:     s.mode = c.mode; break;
            case GovOp::SetBudget:   s.voiceBudget = c.n; break;
            case GovOp::SetPanic:    s.panicLatch = c.on; break;
            case GovOp::SetPlaying:  s.playing = c.on; break;
        }
        return s;
    }

    static bool compensates (GovOp op) noexcept
    {
        return op != GovOp::AcquireSolo && op != GovOp::ReleaseSolo;
    }

    // The conflict tier: returns the next state and how the command resolved. Run on the single applier thread, so
    // the check-then-commit is atomic (no TOCTOU race). Lease acquisition is a guarded reject; the value/mode/gate
    // commands compensate an invariant-violating result into a legal state.
    GovState apply (const GovCmd& c, Resolution& res) const noexcept
    {
        if (c.op == GovOp::AcquireSolo && soloLease != 0 && soloLease != c.by)
        {
            res = Resolution::Rejected;      // the lease is held by another controller
            return *this;
        }
        const GovState n = reduce (c);
        if (n.ok())                    { res = Resolution::Applied;     return n; }
        if (compensates (c.op))        { res = Resolution::Compensated; return n.repair(); }
        res = Resolution::Rejected;
        return *this;
    }
};

} // namespace sidechain
