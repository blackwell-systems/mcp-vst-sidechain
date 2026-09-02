#pragma once
#include <algorithm>
#include <cctype>
#include <string>
#include <unordered_map>
#include <vector>

// ================================================================================================
// sidechain label-prefix section derivation - the PURE core of the host's sectioning (no JUCE dependency), split
// out so it can be unit-tested standalone (cpp/tests/section_derivation_test.cpp). Sectioning.h layers the JUCE
// glue (computeSections) on top. This is the algorithm that must stay in lockstep with the Go catalog's
// sections.go; the C++ unit test covers it directly here, and the gated Go TestSectionLockstep cross-checks the
// host's emitted sections against the Go reference on real plugins.
// ================================================================================================

namespace sidechain
{

inline constexpr int kSectionMinShare = 3;    // a label prefix becomes a section when >= this many params share it

// deriveSectionPerParam: for each label, the longest leading token-prefix shared (as a prefix) by at least
// kSectionMinShare labels, or "other". Labels are split on non-alphanumeric runs with pure-number tokens dropped
// (so "Osc 1 Tune"/"Osc 2 Tune" share "Osc"). Returns a vector parallel to `labels`. Mirrors sections.go.
inline std::vector<std::string> deriveSectionPerParam (const std::vector<std::string>& labels)
{
    auto tokenize = [] (const std::string& label)
    {
        std::vector<std::string> out;
        std::string cur;
        auto flush = [&]
        {
            if (! cur.empty())
            {
                bool allDigits = true;
                for (char ch : cur) if (ch < '0' || ch > '9') { allDigits = false; break; }
                if (! allDigits) out.push_back (cur);   // pure-number tokens are indices, not section words
                cur.clear();
            }
        };
        for (char ch : label)
        {
            const bool alnum = (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9');
            if (alnum) cur += ch; else flush();
        }
        flush();
        return out;
    };

    std::vector<std::vector<std::string>> tokens;
    tokens.reserve (labels.size());
    std::unordered_map<std::string, int> prefixCount;
    for (const auto& lbl : labels)
    {
        auto tk = tokenize (lbl);
        std::string pref;
        for (size_t l = 0; l < tk.size(); ++l)
        {
            if (l) pref += ' ';
            pref += tk[l];
            ++prefixCount[pref];
        }
        tokens.push_back (std::move (tk));
    }

    std::vector<std::string> out (labels.size(), "other");
    for (size_t i = 0; i < tokens.size(); ++i)
    {
        const auto& tk = tokens[i];
        for (size_t l = tk.size(); l >= 1; --l)          // longest qualifying prefix wins
        {
            std::string pref;
            for (size_t k = 0; k < l; ++k) { if (k) pref += ' '; pref += tk[k]; }
            auto it = prefixCount.find (pref);
            if (it != prefixCount.end() && it->second >= kSectionMinShare) { out[i] = pref; break; }
        }
    }
    return out;
}

// Case-insensitive ordering used for the distinct section list (matches the Go catalog's alphabetical groups).
inline bool sectionLess (const std::string& a, const std::string& b)
{
    auto lower = [] (std::string s) { for (auto& ch : s) ch = (char) std::tolower ((unsigned char) ch); return s; };
    const std::string la = lower (a), lb = lower (b);
    return la != lb ? la < lb : a < b;
}

} // namespace sidechain
