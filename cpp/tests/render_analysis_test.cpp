// render_analysis_test.cpp - a minimal, dependency-free unit test for the JUCE-free half of the render
// measurement (cpp/RenderAnalysis.h): peak / rms / crest / silent / clipped from a mono float span. It mirrors
// section_derivation_test.cpp's tiny-assert style: no JUCE, no test framework, compiles standalone
//
//   c++ -std=c++17 -Wall -Wextra -Werror cpp/tests/render_analysis_test.cpp -o rat && ./rat
//
// and is also a CMake/ctest target (render-analysis-test). Returns non-zero on the first failed check. The
// FFT-based measures (centroid / bands) live in the JUCE-linked render path and are validated by the gated E2E,
// not here.

#include "../RenderAnalysis.h"
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <vector>

namespace
{
int g_failures = 0;

void expectTrue (bool cond, const char* what)
{
    if (! cond)
    {
        std::fprintf (stderr, "FAIL %s\n", what);
        ++g_failures;
    }
}

// Assert `got` is within `tol` of `want` (dB comparisons are approximate: FP + the -3.01 dB sine RMS constant).
void expectNear (double got, double want, double tol, const char* what)
{
    if (std::fabs (got - want) > tol)
    {
        std::fprintf (stderr, "FAIL %s: got %.4f want %.4f (tol %.4f)\n", what, got, want, tol);
        ++g_failures;
    }
}

using sidechain::analyzeMono;
using sidechain::BasicMeasurement;

// A full-scale sine (amplitude 1.0): peak about 0 dBFS, rms about -3.01 dBFS (1/sqrt(2)), crest about 3 dB,
// not silent, and clipped true (peak magnitude reaches exactly 1.0 == full scale).
void testFullScaleSine()
{
    constexpr std::size_t n = 48000;
    std::vector<float> buf (n);
    const double twoPi = 6.283185307179586;
    for (std::size_t i = 0; i < n; ++i)
        buf[i] = (float) std::sin (twoPi * 100.0 * (double) i / (double) n);   // 100 whole cycles, no partial

    const BasicMeasurement m = analyzeMono (buf.data(), n);
    expectNear (m.peakDb, 0.0, 0.1, "fullScaleSine.peakDb");
    expectNear (m.rmsDb, -3.01, 0.2, "fullScaleSine.rmsDb");
    expectNear (m.crest, 3.01, 0.2, "fullScaleSine.crest");
    expectTrue (! m.silent, "fullScaleSine.notSilent");
    expectTrue (m.clipped, "fullScaleSine.clipped");   // peak magnitude reaches full scale => clipped at 0 dBFS
}

// Pure silence (all zeros): silent true, not clipped, levels at the floor, crest 0.
void testSilence()
{
    constexpr std::size_t n = 4096;
    std::vector<float> buf (n, 0.0f);
    const BasicMeasurement m = analyzeMono (buf.data(), n);
    expectTrue (m.silent, "silence.silent");
    expectTrue (! m.clipped, "silence.notClipped");
    expectNear (m.crest, 0.0, 1.0e-9, "silence.crestZero");
    expectTrue (m.peakDb <= sidechain::kDbFloor + 1.0e-6, "silence.peakFloor");
}

// A louder buffer has a higher RMS than a quieter one of the same shape (monotonicity of the level measure).
void testLouderHasHigherRms()
{
    constexpr std::size_t n = 2048;
    std::vector<float> quiet (n), loud (n);
    const double twoPi = 6.283185307179586;
    for (std::size_t i = 0; i < n; ++i)
    {
        const float s = (float) std::sin (twoPi * 8.0 * (double) i / (double) n);
        quiet[i] = 0.1f * s;
        loud[i]  = 0.5f * s;
    }
    const BasicMeasurement q = analyzeMono (quiet.data(), n);
    const BasicMeasurement l = analyzeMono (loud.data(), n);
    expectTrue (l.rmsDb > q.rmsDb, "louder.higherRms");
    expectTrue (l.peakDb > q.peakDb, "louder.higherPeak");
    expectTrue (! q.silent && ! l.silent, "louder.neitherSilent");
    expectTrue (! q.clipped && ! l.clipped, "louder.neitherClipped");   // both below full scale
}

// A near-silent low-amplitude signal (below kSilenceThreshold) is reported silent even though it is not exactly 0.
void testBelowThresholdIsSilent()
{
    constexpr std::size_t n = 512;
    std::vector<float> buf (n, 1.0e-5f);   // below kSilenceThreshold (1e-4)
    const BasicMeasurement m = analyzeMono (buf.data(), n);
    expectTrue (m.silent, "belowThreshold.silent");
}

// Empty / null spans report silence without touching memory.
void testEmpty()
{
    const BasicMeasurement e = analyzeMono (nullptr, 0);
    expectTrue (e.silent && ! e.clipped, "empty.silent");
    std::vector<float> none;
    const BasicMeasurement z = analyzeMono (none.data(), 0);
    expectTrue (z.silent, "empty.zeroLen");
}

// ================================================================================================
// Tier 2.5: analyzeEnvelope tests (JUCE-free, same file, same tiny-assert style).
// ================================================================================================

using sidechain::analyzeEnvelope;
using sidechain::EnvStats;
using sidechain::estimateF0;

// A periodic sine envelope sampled at fsEnv=40 Hz for ~2 s (80 frames) at 2 Hz.
// Expected: rateHz within ~10% of 2.0, regular=true, depth ~ 2*amplitude, confidence > 0.5.
void testSineEnvelope()
{
    // 2 Hz sine, amplitude 1.0, fsEnv = 40 Hz, 80 frames (2.0 s of envelope).
    constexpr std::size_t n = 80;
    constexpr double fsEnv  = 40.0;
    constexpr double rate   = 2.0;
    constexpr float  amp    = 1.0f;
    const double twoPi = 6.283185307179586;

    std::vector<float> env (n);
    for (std::size_t i = 0; i < n; ++i)
        env[i] = amp * (float) std::sin (twoPi * rate * (double) i / fsEnv);

    const EnvStats s = analyzeEnvelope (env.data(), n, fsEnv);

    // Rate should be within 10% of 2 Hz.
    expectTrue (std::fabs (s.rateHz - rate) / rate < 0.10, "sineEnv.rateHz ~2 Hz");
    // depth should be approximately 2 * amplitude (peak-to-peak).
    expectNear (s.depth, (double) (2.0f * amp), 0.15, "sineEnv.depth ~2.0");
    expectTrue (s.regular, "sineEnv.regular");
    expectTrue (s.confidence > 0.5, "sineEnv.confidence>0.5");
}

// A linear ramp envelope: aperiodic, so regular=false and confidence should be low.
// depth = span of the ramp.
void testRampEnvelope()
{
    constexpr std::size_t n    = 60;
    constexpr double      fsEnv = 40.0;
    std::vector<float> env (n);
    for (std::size_t i = 0; i < n; ++i)
        env[i] = (float) i / (float) (n - 1);   // 0 .. 1 linear ramp

    const EnvStats s = analyzeEnvelope (env.data(), n, fsEnv);
    expectTrue (! s.regular, "rampEnv.notRegular");
    // depth should be approximately 1 (span of the 0..1 ramp).
    expectNear (s.depth, 1.0, 0.05, "rampEnv.depth~1.0");
}

// White-noise envelope: irregular, should not be detected as regular.
void testNoiseEnvelope()
{
    // Pseudo-random noise using a simple LCG so the test is deterministic.
    constexpr std::size_t n    = 80;
    constexpr double      fsEnv = 40.0;
    std::vector<float> env (n);
    uint32_t state = 0xDEADBEEFu;
    for (std::size_t i = 0; i < n; ++i)
    {
        state = state * 1664525u + 1013904223u;
        env[i] = (float) (int32_t) state * (1.0f / 2147483648.0f);   // in [-1, 1]
    }
    const EnvStats s = analyzeEnvelope (env.data(), n, fsEnv);
    // White noise has no strong autocorrelation peak so regular should be false.
    expectTrue (! s.regular, "noiseEnv.notRegular");
}

// Flat (constant) envelope: depth ~ 0, regular=false, confidence 0.
void testFlatEnvelope()
{
    constexpr std::size_t n    = 40;
    constexpr double      fsEnv = 40.0;
    std::vector<float> env (n, 0.5f);   // constant

    const EnvStats s = analyzeEnvelope (env.data(), n, fsEnv);
    expectNear (s.depth, 0.0, 1.0e-4, "flatEnv.depth~0");
    expectTrue (! s.regular, "flatEnv.notRegular");
    expectNear (s.confidence, 0.0, 1.0e-9, "flatEnv.confidence0");
    expectNear (s.rateHz, 0.0, 1.0e-9, "flatEnv.rateHz0");
}

// Too-short span (n < 4): returns default (no-op) without crashing.
void testTooShortEnvelope()
{
    constexpr double fsEnv = 40.0;
    std::vector<float> env = { 0.1f, 0.9f, 0.1f };   // n=3 < 4
    const EnvStats s = analyzeEnvelope (env.data(), env.size(), fsEnv);
    // Should not crash; result is all-zero defaults.
    expectNear (s.rateHz, 0.0, 1.0e-9, "shortEnv.rateHz0");
    expectTrue (! s.regular, "shortEnv.notRegular");
    expectNear (s.confidence, 0.0, 1.0e-9, "shortEnv.confidence0");
}

} // namespace

// estimateF0: a pure sine at a known frequency should be recovered within a small tolerance; silence is unvoiced.
void testEstimateF0Sine()
{
    constexpr double sr = 48000.0;
    constexpr std::size_t n = 1200;   // 25 ms frame
    const double twoPi = 6.283185307179586;
    for (double f0 : { 130.81, 220.0, 440.0 })   // C3, A3, A4
    {
        std::vector<float> x (n);
        for (std::size_t i = 0; i < n; ++i)
            x[i] = (float) std::sin (twoPi * f0 * (double) i / sr);
        const double got = estimateF0 (x.data(), n, sr);
        expectTrue (std::fabs (got - f0) / f0 < 0.03, "estimateF0 within 3% of the tone");
    }
}

void testEstimateF0Silence()
{
    constexpr std::size_t n = 1200;
    std::vector<float> x (n, 0.0f);
    expectNear (estimateF0 (x.data(), n, 48000.0), 0.0, 1e-9, "estimateF0(silence) == 0 (unvoiced)");
}

int main()
{
    testFullScaleSine();
    testSilence();
    testLouderHasHigherRms();
    testBelowThresholdIsSilent();
    testEmpty();

    // Tier 2.5: envelope analysis tests.
    testSineEnvelope();
    testRampEnvelope();
    testNoiseEnvelope();
    testFlatEnvelope();
    testTooShortEnvelope();

    // Pitch detection (for vibrato tracking).
    testEstimateF0Sine();
    testEstimateF0Silence();

    if (g_failures == 0)
    {
        std::printf ("render-analysis-test: all checks passed\n");
        return 0;
    }
    std::fprintf (stderr, "render-analysis-test: %d check(s) failed\n", g_failures);
    return 1;
}
