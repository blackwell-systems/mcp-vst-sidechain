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
    juce::juce_recommended_config_flags
    juce::juce_recommended_lto_flags
    juce::juce_recommended_warning_flags)

# Minimal unit test for the pure label-prefix section derivation (SectionDerivation.h). It has NO JUCE dependency,
# so it is a plain executable that builds in a second and needs no plugin - run it via ctest, or compile the .cpp
# directly (see its header). This gives the derivation C++ unit coverage on the side where it now lives; the gated
# Go TestSectionLockstep separately proves the host output matches the Go reference on real plugins.
enable_testing()
add_executable(section-derivation-test tests/section_derivation_test.cpp)
set_target_properties(section-derivation-test PROPERTIES CXX_STANDARD 17 CXX_STANDARD_REQUIRED ON)
add_test(NAME section-derivation COMMAND section-derivation-test)
