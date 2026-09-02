#pragma once
#include <juce_audio_processors/juce_audio_processors.h>
#include <algorithm>
#include <functional>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <vector>
#include "SectionDerivation.h"

// ================================================================================================
// sidechain::computeSections - the ONE place the host computes a plugin's navigable sections. Both the catalog
// emitter (Host::enumerateCatalog, which writes a per-param `section`) and the C3 governed layer (ControlServer's
// leasable sections, via JucePluginBridge::sectionGroups) use it, so there is a single source of truth for
// sectioning on the host.
//
// A param's EFFECTIVE section is: its parameter-tree group (VST3 unit / AU clump) when the plugin exposes one;
// otherwise a section DERIVED from shared label prefixes (SectionDerivation.h, unit-tested standalone); otherwise
// "other". The derivation mirrors the Go catalog's sections.go - the Go side prefers the host-emitted `section`
// and keeps its own derivation only as a fallback and as the reference oracle the gated TestSectionLockstep
// cross-checks this against, so the two stay provably in lockstep.
// ================================================================================================

namespace sidechain
{

// The result of sectioning a plugin: the raw tree group and the effective section for each automatable parameter,
// plus the distinct leasable sections (non-"other", sorted).
struct SectionMap
{
    std::unordered_map<const juce::AudioProcessorParameter*, std::string> rawGroup;   // tree group, "" if none
    std::unordered_map<const juce::AudioProcessorParameter*, std::string> effective;  // group / derived / "other"
    std::vector<std::string>                                             leasable;    // distinct non-"other", sorted
};

inline SectionMap computeSections (const juce::AudioProcessor& proc)
{
    SectionMap sm;

    // 1. Raw parameter-tree groups (VST3 units / AU clumps): the immediate parent group name per param.
    std::function<void (const juce::AudioProcessorParameterGroup&)> walk =
        [&] (const juce::AudioProcessorParameterGroup& g)
        {
            for (auto* node : g)
            {
                if (auto* p = node->getParameter())
                    sm.rawGroup[p] = g.getName().toStdString();
                else if (auto* sub = node->getGroup())
                    walk (*sub);
            }
        };
    walk (proc.getParameterTree());

    // 2. Grouped iff any automatable param carries a non-empty tree group.
    bool grouped = false;
    for (auto* p : proc.getParameters())
        if (p->isAutomatable())
        {
            auto it = sm.rawGroup.find (p);
            if (it != sm.rawGroup.end() && ! it->second.empty()) { grouped = true; break; }
        }

    std::unordered_set<std::string> leaseSet;
    if (grouped)
    {
        for (auto* p : proc.getParameters())
        {
            if (! p->isAutomatable()) continue;
            auto it = sm.rawGroup.find (p);
            std::string g = (it != sm.rawGroup.end() && ! it->second.empty()) ? it->second : "other";
            sm.effective[p] = g;
            if (g != "other") leaseSet.insert (g);
        }
    }
    else
    {
        // Flat plugin: derive sections from shared label prefixes.
        std::vector<juce::AudioProcessorParameter*> autos;
        std::vector<std::string> labels;
        for (auto* p : proc.getParameters())
            if (p->isAutomatable()) { autos.push_back (p); labels.push_back (p->getName (256).toStdString()); }
        const auto perParam = deriveSectionPerParam (labels);
        for (size_t i = 0; i < autos.size(); ++i)
        {
            sm.effective[autos[i]] = perParam[i];
            if (perParam[i] != "other") leaseSet.insert (perParam[i]);
        }
    }

    sm.leasable.assign (leaseSet.begin(), leaseSet.end());
    std::sort (sm.leasable.begin(), sm.leasable.end(), sectionLess);
    return sm;
}

} // namespace sidechain
