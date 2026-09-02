#pragma once
#include <cmath>
#include <cstddef>

// ================================================================================================
// sidechain render analysis - the PURE, JUCE-free half of the offline-render measurement (peak / rms / crest /
// silent / clipped), computed from a flat float sample span (the rendered output summed to mono). It is split out
// exactly like SectionDerivation.h so it can be unit-tested standalone (cpp/tests/render_analysis_test.cpp) with
// no JUCE dependency and no test framework. The FFT-based measures (spectral centroid, 3-band energy) need
// juce::dsp::FFT and so live in the JUCE-linked render path (JucePluginBridge::renderAndMeasure), where the
// gated real-host E2E validates them; they are NOT part of this pure core.
//
// Levels are dBFS: 0 dB is full scale (a sample magnitude of 1.0). A pure full-scale sine has peak 0 dBFS and
// RMS about -3.01 dBFS (1/sqrt(2)); crest = peak - rms is therefore about 3 dB for that sine. Silence and a
// dead patch are reported by `silent` (peak below a small threshold); an over-hot render by `clipped` (peak at
// or above full scale).
// ================================================================================================

namespace sidechain
{

// The JUCE-free portion of a render measurement, filled by analyzeMono from a mono sample span.
struct BasicMeasurement
{
    double peakDb = -160.0;   // max |sample|, dBFS (peakDbFloor for pure silence)
    double rmsDb  = -160.0;   // RMS level, dBFS (rmsDbFloor for pure silence)
    double crest  = 0.0;      // peak - rms, dB (transient-ness); 0 when silent
    bool   silent = true;     // peak magnitude below kSilenceThreshold (dead patch / no output)
    bool   clipped = false;   // peak magnitude at or above full scale (>= 0 dBFS)
};

// Below this linear magnitude the output is treated as silence (about -80 dBFS). A dead patch or a render with no
// signal lands here; it also keeps a log10 of a near-zero value out of the reported numbers.
inline constexpr double kSilenceThreshold = 1.0e-4;

// The floor reported for peak / rms when the signal is (near) silent, so a JSON consumer never sees -inf.
inline constexpr double kDbFloor = -160.0;

// Convert a linear magnitude (>= 0) to dBFS, clamped to kDbFloor so silence is a finite number, not -inf.
inline double linearToDb (double linear)
{
    if (linear <= 0.0)
        return kDbFloor;
    const double db = 20.0 * std::log10 (linear);
    return db < kDbFloor ? kDbFloor : db;
}

// Analyze a mono float span: peak (dBFS), rms (dBFS), crest (peak - rms, dB), silent, clipped. Pure and
// allocation-free, so it is trivially testable and safe to call on the message thread. `n == 0` reports silence.
inline BasicMeasurement analyzeMono (const float* samples, std::size_t n)
{
    BasicMeasurement m;
    if (samples == nullptr || n == 0)
        return m;   // defaults: silent, floor levels

    double peakLinear = 0.0;
    double sumSquares = 0.0;
    for (std::size_t i = 0; i < n; ++i)
    {
        const double s = (double) samples[i];
        const double mag = std::fabs (s);
        if (mag > peakLinear)
            peakLinear = mag;
        sumSquares += s * s;
    }

    const double rmsLinear = std::sqrt (sumSquares / (double) n);

    m.silent  = (peakLinear < kSilenceThreshold);
    m.clipped = (peakLinear >= 1.0);
    m.peakDb  = linearToDb (peakLinear);
    m.rmsDb   = linearToDb (rmsLinear);
    // Crest is peak-over-rms in dB (a transient-ness proxy). Undefined/meaningless when silent, so report 0.
    m.crest   = m.silent ? 0.0 : (m.peakDb - m.rmsDb);
    return m;
}

} // namespace sidechain
