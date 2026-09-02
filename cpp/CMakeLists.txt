# CMakeLists.txt - builds the headless Sidechain host (sidechain-host), a JUCE console app that loads a VST3/AU
# child plugin and serves the ControlServer wire protocol on loopback.
#
# JUCE is pulled via FetchContent (no submodule, no system install). A first configure downloads JUCE, so it is
# network- and time-heavy; pin the tag for reproducibility.
#
#   cmake -B build -DCMAKE_BUILD_TYPE=Release
#   cmake --build build --target sidechain-host
#
# Then:  ./build/sidechain-host_artefacts/Release/sidechain-host --plugin /path/to/Plugin.vst3

cmake_minimum_required(VERSION 3.22)
project(mcp-vst-sidechain-host VERSION 0.1.0 LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

include(FetchContent)

# Pin a JUCE release. JUCE 8 has the mature AudioPluginFormatManager VST3/AU hosting this project depends on.
set(SIDECHAIN_JUCE_TAG "8.0.4" CACHE STRING "JUCE git tag to fetch")
FetchContent_Declare(
    JUCE
    GIT_REPOSITORY https://github.com/juce-framework/JUCE.git
    GIT_TAG        ${SIDECHAIN_JUCE_TAG}
    GIT_SHALLOW    TRUE
)
FetchContent_MakeAvailable(JUCE)

juce_add_console_app(sidechain-host
    PRODUCT_NAME "sidechain-host")

target_sources(sidechain-host PRIVATE
    main.cpp
    Host.cpp)

target_compile_definitions(sidechain-host PRIVATE
    JUCE_PLUGINHOST_VST3=1
    JUCE_PLUGINHOST_AU=1          # honoured on macOS; a no-op elsewhere
    JUCE_USE_CURL=0
    JUCE_WEB_BROWSER=0
    JUCE_STANDALONE_APPLICATION=1)

target_link_libraries(sidechain-host PRIVATE
    juce::juce_audio_processors
    juce::juce_audio_utils
    juce::juce_audio_devices
    juce::juce_dsp                     # juce::dsp::FFT for the render path's spectral centroid / band energy
    juce::juce_recommended_config_flags
    juce::juce_recommended_lto_flags
    juce::juce_recommended_warning_flags)

# Warnings-as-errors on OUR two translation units ONLY (main.cpp, Host.cpp), so warnings in our code fail the build
# and cannot rot. It is NOT applied target-wide: juce_add_console_app compiles JUCE's own module sources into the
# target, and JUCE is not warning-clean under -Werror (e.g. a deprecated wstring_convert in juce_audio_processors),
# which is why a target-wide flag was deferred. Our TUs still pull in JUCE headers, so this also covers the headers
# as we use them. JUCE is PINNED (SIDECHAIN_JUCE_TAG); the baseline is clean across ubuntu/macOS/Windows.
set_source_files_properties(main.cpp Host.cpp PROPERTIES COMPILE_OPTIONS
    "$<$<CXX_COMPILER_ID:MSVC>:/WX>$<$<NOT:$<CXX_COMPILER_ID:MSVC>>:-Werror>")

# Minimal unit test for the pure label-prefix section derivation (SectionDerivation.h). It has NO JUCE dependency,
# so it is a plain executable that builds in a second and needs no plugin - run it via ctest, or compile the .cpp
# directly (see its header). This gives the derivation C++ unit coverage on the side where it now lives; the gated
# Go TestSectionLockstep separately proves the host output matches the Go reference on real plugins.
enable_testing()
add_executable(section-derivation-test tests/section_derivation_test.cpp)
set_target_properties(section-derivation-test PROPERTIES CXX_STANDARD 17 CXX_STANDARD_REQUIRED ON)
add_test(NAME section-derivation COMMAND section-derivation-test)

# Minimal unit test for the pure, JUCE-free half of the render measurement (RenderAnalysis.h: peak/rms/crest/
# silent/clipped). Same pattern as above: a plain standalone executable, no JUCE, no plugin. The FFT-based
# centroid/bands live in the JUCE-linked render path and are validated by the gated E2E, not here.
add_executable(render-analysis-test tests/render_analysis_test.cpp)
set_target_properties(render-analysis-test PROPERTIES CXX_STANDARD 17 CXX_STANDARD_REQUIRED ON)
add_test(NAME render-analysis COMMAND render-analysis-test)
