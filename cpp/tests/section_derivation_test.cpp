// section_derivation_test.cpp - a minimal, dependency-free unit test for the host's label-prefix section
// derivation (cpp/SectionDerivation.h). It mirrors the Go catalog's sections_test.go cases so both sides of the
// (lockstep) algorithm are unit-tested in their own language. No JUCE, no test framework: it compiles standalone
//
//   c++ -std=c++17 -Wall -Wextra cpp/tests/section_derivation_test.cpp -o sdt && ./sdt
//
// and is also a CMake/ctest target (section-derivation-test). Returns non-zero on the first failed check.

#include "../SectionDerivation.h"
#include <cstdio>
#include <string>
#include <vector>

namespace
{
int g_failures = 0;

void expectEq (const std::string& got, const std::string& want, const char* what)
{
    if (got != want)
    {
        std::fprintf (stderr, "FAIL %s: got \"%s\" want \"%s\"\n", what, got.c_str(), want.c_str());
        ++g_failures;
    }
}

void expectTrue (bool cond, const char* what)
{
    if (! cond)
    {
        std::fprintf (stderr, "FAIL %s\n", what);
        ++g_failures;
    }
}

using sidechain::deriveSectionPerParam;
using sidechain::sectionLess;

// Three params sharing the full token prefix take that whole prefix, not just the first token: "Osc 1 Tune" etc.
// tokenize to ["Osc","Tune"] (the index is dropped), and "Osc Tune" is shared by all three.
void testSharedFullPrefix()
{
    auto s = deriveSectionPerParam ({ "Osc 1 Tune", "Osc 2 Tune", "Osc 3 Tune" });
    expectEq (s.at (0), "Osc Tune", "sharedFullPrefix[0]");
    expectEq (s.at (1), "Osc Tune", "sharedFullPrefix[1]");
    expectEq (s.at (2), "Osc Tune", "sharedFullPrefix[2]");
}

// The k threshold: a prefix shared by only two params is NOT promoted (both -> other); by three, it is.
void testThreshold()
{
    auto two = deriveSectionPerParam ({ "Filter A", "Filter B" });
    expectEq (two.at (0), "other", "threshold.two[0]");
    expectEq (two.at (1), "other", "threshold.two[1]");

    auto three = deriveSectionPerParam ({ "Filter A", "Filter B", "Filter C" });
    expectEq (three.at (0), "Filter", "threshold.three[0]");
    expectEq (three.at (1), "Filter", "threshold.three[1]");
    expectEq (three.at (2), "Filter", "threshold.three[2]");
}

// Longest qualifying prefix wins: "Amp Env *" (3 share "Amp Env") beats the shorter "Amp" (4 share), while a
// sibling that only shares "Amp" falls back to "Amp".
void testLongestQualifying()
{
    auto s = deriveSectionPerParam ({ "Amp Env Attack", "Amp Env Decay", "Amp Env Sustain", "Amp Gain" });
    expectEq (s.at (0), "Amp Env", "longest.env[0]");
    expectEq (s.at (1), "Amp Env", "longest.env[1]");
    expectEq (s.at (2), "Amp Env", "longest.env[2]");
    expectEq (s.at (3), "Amp",     "longest.gain");
}

// No qualifying shared prefix -> other; a lone label among grouped ones -> other.
void testNoSharedPrefix()
{
    auto s = deriveSectionPerParam ({ "Portamento", "Voices", "Glide", "Filter A", "Filter B", "Filter C" });
    expectEq (s.at (0), "other",  "noShared.portamento");
    expectEq (s.at (1), "other",  "noShared.voices");
    expectEq (s.at (2), "other",  "noShared.glide");
    expectEq (s.at (3), "Filter", "noShared.filterA");
}

// Pure-number tokens are dropped (indices, not section words), and a label with only numbers has no tokens.
void testNumberTokens()
{
    auto s = deriveSectionPerParam ({ "LFO 1", "LFO 2", "LFO 3", "128" });
    expectEq (s.at (0), "LFO",   "numbers.lfo1");   // "LFO 1" -> ["LFO"], shared by three
    expectEq (s.at (1), "LFO",   "numbers.lfo2");
    expectEq (s.at (2), "LFO",   "numbers.lfo3");
    expectEq (s.at (3), "other", "numbers.pureNumber");
}

// A real slice of TAL-NoiseMaker labels derives the same sections the host emits (Amp, Filter, Osc, Osc Volume).
void testTalSlice()
{
    auto s = deriveSectionPerParam ({
        "Filter Cutoff", "Filter Resonance", "Filter Attack",
        "Amp Attack", "Amp Decay", "Amp Sustain",
        "Osc 1 Volume", "Osc 2 Volume", "Osc 3 Volume",
    });
    expectEq (s.at (0), "Filter",     "tal.filterCutoff");
    expectEq (s.at (3), "Amp",        "tal.ampAttack");
    expectEq (s.at (6), "Osc Volume", "tal.oscVolume");
}

// sectionLess is case-insensitive with an original-string tiebreak.
void testSectionLess()
{
    expectTrue (sectionLess ("amp", "Filter"),  "less.ampBeforeFilter"); // a < f, case-insensitive
    expectTrue (! sectionLess ("Filter", "amp"), "less.notFilterBeforeAmp");
    expectTrue (sectionLess ("Amp", "amp"),     "less.tiebreakUpperFirst"); // equal lower, 'A' < 'a'
}
} // namespace

int main()
{
    testSharedFullPrefix();
    testThreshold();
    testLongestQualifying();
    testNoSharedPrefix();
    testNumberTokens();
    testTalSlice();
    testSectionLess();

    if (g_failures == 0)
    {
        std::printf ("section-derivation-test: all checks passed\n");
        return 0;
    }
    std::fprintf (stderr, "section-derivation-test: %d check(s) failed\n", g_failures);
    return 1;
}
