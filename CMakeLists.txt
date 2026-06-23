# Agent Kate superbuild: builds the C++/Qt UI and the Go orchestration core.
cmake_minimum_required(VERSION 3.20)
project(AgentKate VERSION 0.1.0 LANGUAGES CXX)

# Emit build/compile_commands.json so clangd (Agent Kate's own LSP) resolves the
# Qt6/KF6 include paths and gives accurate diagnostics for this project.
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

# --- KDE/ECM install-dir conventions ---------------------------------------
# Load ECM + KDEInstallDirs before anything else that might pull in
# GNUInstallDirs (Qt6, find_package, etc.). This keeps KDE_INSTALL_* from
# being seeded by CMAKE_INSTALL_* and silences the ECM warnings.
find_package(ECM 6.0 NO_MODULE REQUIRED)
list(APPEND CMAKE_MODULE_PATH ${ECM_MODULE_PATH})
include(KDEInstallDirs)
include(ECMInstallIcons)

# --- Derived version: MAJOR.MINOR.<total-commits> --------------------------
# MAJOR.MINOR come from project() above; the patch component is the total number
# of commits reachable from HEAD (git rev-list --count). This makes the version
# monotonic and merge-strategy independent: every commit raises it by one, and a
# branch that has been merged into main is always <= main (main contains every
# merged branch's commits plus the merge). NB: --first-parent must NOT be used —
# with merge-commit PRs it skips the merged branch's commits, so main would count
# only +1 per PR and feature-branch builds would out-number main.
# Override with -DAGENTKATE_VERSION=... (packaging passes the version explicitly,
# since the unpacked source tarball it builds from has no .git directory).
if(NOT AGENTKATE_VERSION)
    find_package(Git QUIET)
    if(GIT_FOUND)
        execute_process(
            COMMAND ${GIT_EXECUTABLE} rev-list --count HEAD
            WORKING_DIRECTORY ${CMAKE_SOURCE_DIR}
            OUTPUT_VARIABLE _ak_commit_count
            OUTPUT_STRIP_TRAILING_WHITESPACE
            ERROR_QUIET)
    endif()
    if(_ak_commit_count)
        set(AGENTKATE_VERSION "${PROJECT_VERSION_MAJOR}.${PROJECT_VERSION_MINOR}.${_ak_commit_count}")
    else()
        set(AGENTKATE_VERSION "${PROJECT_VERSION}")
    endif()
endif()
message(STATUS "Agent Kate version: ${AGENTKATE_VERSION}")

# --- Go orchestration core (akcore) ----------------------------------------
# Built by a custom target that shells out to the Go toolchain. The binary is
# placed next to the agentkate executable so the UI can spawn it directly. The
# derived version is stamped in via -ldflags so akcore reports the same number
# the package does. (Quoted as one argument so the space in the -X value
# survives VERBATIM.)
find_program(GO_EXECUTABLE go REQUIRED)

add_custom_target(akcore ALL
    COMMAND ${GO_EXECUTABLE} build "-ldflags=-X main.version=${AGENTKATE_VERSION}"
            -o ${CMAKE_BINARY_DIR}/akcore ./cmd/akcore
    WORKING_DIRECTORY ${CMAKE_SOURCE_DIR}/core
    COMMENT "Building akcore (Go orchestration core)"
    VERBATIM
    USES_TERMINAL
)

# Enable CTest at the top level so `ctest` in the build root aggregates the
# tests registered in subdirectories (e.g. ui/tests). Must precede the
# add_subdirectory() calls that register tests.
enable_testing()

# --- C++/Qt UI (agentkate) -------------------------------------------------
# The UI subdirectory pulls in ECM/KDEInstallDirs, which must be the first
# install-dirs module to load so KDE_INSTALL_* aren't seeded from CMAKE_INSTALL_*.
add_subdirectory(ui)

# akcore install goes here (after ui/) so KDE_INSTALL_BINDIR is defined.
install(PROGRAMS ${CMAKE_BINARY_DIR}/akcore
        DESTINATION ${KDE_INSTALL_BINDIR})
