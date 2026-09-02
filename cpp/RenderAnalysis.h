#pragma once
#include <cmath>
#include <cstddef>
#include <string>
#include <vector>

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
//
// Tier 2.5 extension: analyzeEnvelope (further down) is the pure envelope-periodicity estimator used by the
// temporal (modulation-aware) measurement path. It is JUCE-free and unit-testable on synthetic envelopes.
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

// ================================================================================================
// Tier 2.5: envelope periodicity analysis (pure, JUCE-free, allocation via local std::vector only).
//
// analyzeEnvelope takes a SHORT TIME SERIES (the per-frame envelope of a metric, e.g. centroid in Hz over frames,
// or RMS in dB over frames) sampled at `fsEnv` (Hz, the frame rate = sampleRate / frameLen), and estimates the
// dominant periodicity of any modulation present (LFO rate, tremolo, sweep, etc.).
//
// Algorithm:
//   1. Compute mean; subtract it (detrend). depth = max(env) - min(env).
//   2. If depth is negligible (< kEnvEpsilon) or n < 4: return rateHz=0, depth=depth, regular=false, confidence=0.
//   3. Build the normalized autocorrelation (NACF) for lags 2..n/2 (normalized by lag-0 energy).
//   4. Find the first LOCAL MAXIMUM in the NACF after it has first dropped below kAcfDropThreshold (0.2).
//      This correctly identifies the fundamental period of a periodic signal (avoiding the trivially high values
//      at very short lags) while also rejecting a monotonically decaying ACF like a linear ramp (which never
//      drops below the threshold before reaching a local maximum beyond the drop).
//      If the NACF never drops below the threshold, no periodic peak is found; rateHz=0, regular=false.
//      Otherwise the lag of the first local maximum after the drop is the period: rateHz = fsEnv / bestLag.
//   5. regular = (peakNACF >= 0.5) AND at least 2 cycles fit (n / bestLag >= 2).
//   6. confidence = clamp(peakNACF * min(1, (n/bestLag)/4), 0, 1). More cycles and a sharper peak raise it.
//
// Returned depth units match the input units: Hz for a centroid envelope, dB for an RMS envelope.
// ================================================================================================

// Epsilon below which a depth is treated as negligible (avoids false detections on flat envelopes).
inline constexpr float kEnvEpsilon = 1.0e-6f;

// NACF must drop below this before we start looking for a periodic peak. Keeps the algorithm from mistaking the
// high short-lag correlation of a smooth monotonic signal (ramp, DC drift) for periodicity.
inline constexpr double kAcfDropThreshold = 0.2;

// The result of analyzeEnvelope, describing the dominant periodicity of an envelope time series.
struct EnvStats
{
    double rateHz     = 0.0;   // dominant periodic rate in Hz (fsEnv / bestLag); 0 if no clear periodicity
    double depth      = 0.0;   // excursion of the envelope = max - min (original units: Hz for centroid, dB for rms)
    double confidence = 0.0;   // 0..1 estimate of how reliable the rate estimate is
    bool   regular    = false; // true iff the autocorrelation peak is sharp (>= 0.5) AND >= 2 cycles fit
};

// Analyze the periodicity of a float envelope (length n, frame rate fsEnv Hz). Pure, allocation-limited to a
// local autocorrelation vector of length n/2. Safe to call from any thread. `env` must not be null when n > 0.
inline EnvStats analyzeEnvelope (const float* env, std::size_t n, double fsEnv)
{
    EnvStats result;
    if (env == nullptr || n < 4 || fsEnv <= 0.0)
        return result;

    // Find the min/max to compute depth before any detrending.
    float envMin = env[0], envMax = env[0];
    double sum = 0.0;
    for (std::size_t i = 0; i < n; ++i)
    {
        if (env[i] < envMin) envMin = env[i];
        if (env[i] > envMax) envMax = env[i];
        sum += (double) env[i];
    }
    result.depth = (double) (envMax - envMin);

    // Trivially flat (no modulation): depth below epsilon => confidence 0, rateHz 0.
    if ((float) result.depth < kEnvEpsilon)
        return result;   // depth is already set; regular/rateHz/confidence stay at defaults

    const double mean = sum / (double) n;

    // Detrend: subtract the mean so a DC offset does not dominate the autocorrelation.
    std::vector<double> x (n);
    for (std::size_t i = 0; i < n; ++i)
        x[i] = (double) env[i] - mean;

    // Lag-0 energy (the denominator for normalized autocorrelation).
    double lag0 = 0.0;
    for (std::size_t i = 0; i < n; ++i)
        lag0 += x[i] * x[i];
    if (lag0 <= 0.0)
        return result;   // all values equal after detrending; no periodicity

    // Build the normalized autocorrelation for lags 2..n/2.
    const std::size_t maxLag = n / 2;
    std::vector<double> nacf ((std::size_t) (maxLag + 1), 0.0);
    for (std::size_t lag = 2; lag <= maxLag; ++lag)
    {
        double acf = 0.0;
        for (std::size_t i = 0; i + lag < n; ++i)
            acf += x[i] * x[i + lag];
        nacf[lag] = acf / lag0;
    }

    // Find the first local maximum AFTER the NACF has dropped below kAcfDropThreshold.
    // Phase 1: scan forward until nacf[lag] < kAcfDropThreshold (the signal is "far from perfect correlation").
    // Phase 2: once below the threshold, find the first lag where nacf[lag] is a local maximum (nacf[lag-1]
    //          <= nacf[lag] >= nacf[lag+1]) and nacf[lag] > 0. That is the period candidate.
    bool dropped = false;
    std::size_t bestLag = 0;
    double bestAcf = 0.0;
    for (std::size_t lag = 2; lag <= maxLag; ++lag)
    {
        if (! dropped)
        {
            if (nacf[lag] < kAcfDropThreshold)
                dropped = true;
            continue;   // still in the initial high-correlation region; skip
        }
        // We are past the drop. Look for the first local max (ascending then descending, or peak at a boundary).
        // We need lag+1 to check descent; stop at maxLag-1 to keep lag+1 valid.
        if (lag + 1 > maxLag)
            break;
        if (nacf[lag] >= nacf[lag - 1] && nacf[lag] >= nacf[lag + 1] && nacf[lag] > 0.0)
        {
            bestLag = lag;
            bestAcf = nacf[lag];
            break;   // take the FIRST such peak (the fundamental period, not a harmonic)
        }
    }

    if (bestLag == 0)
        return result;   // no periodic peak found after the drop (ramp, noise, short signal)

    // Rate estimate from the fundamental-period lag.
    result.rateHz = fsEnv / (double) bestLag;

    // How many cycles fit in the buffer?
    const double cycles = (double) n / (double) bestLag;

    // regular: the peak must be strong (>= 0.5) AND at least 2 full cycles fit.
    result.regular = (bestAcf >= 0.5) && (cycles >= 2.0);

    // confidence: ACF peak strength * cycle-count saturation (4 cycles = full credit).
    const double cycleFactor = cycles / 4.0 < 1.0 ? cycles / 4.0 : 1.0;
    const double rawConfidence = bestAcf * cycleFactor;
    result.confidence = rawConfidence < 0.0 ? 0.0 : (rawConfidence > 1.0 ? 1.0 : rawConfidence);

    return result;
}

} // namespace sidechain
