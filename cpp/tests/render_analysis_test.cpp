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
} // namespace

int main()
{
    testFullScaleSine();
    testSilence();
    testLouderHasHigherRms();
    testBelowThresholdIsSilent();
    testEmpty();

    if (g_failures == 0)
    {
        std::printf ("render-analysis-test: all checks passed\n");
        return 0;
    }
    std::fprintf (stderr, "render-analysis-test: %d check(s) failed\n", g_failures);
    return 1;
}
