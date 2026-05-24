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

# --- Go orchestration core (akcore) ----------------------------------------
# Built by a custom target that shells out to the Go toolchain. The binary is
# placed next to the agentkate executable so the UI can spawn it directly.
find_program(GO_EXECUTABLE go REQUIRED)

add_custom_target(akcore ALL
    COMMAND ${GO_EXECUTABLE} build -o ${CMAKE_BINARY_DIR}/akcore ./cmd/akcore
    WORKING_DIRECTORY ${CMAKE_SOURCE_DIR}/core
    COMMENT "Building akcore (Go orchestration core)"
    VERBATIM
    USES_TERMINAL
)

# --- C++/Qt UI (agentkate) -------------------------------------------------
# The UI subdirectory pulls in ECM/KDEInstallDirs, which must be the first
# install-dirs module to load so KDE_INSTALL_* aren't seeded from CMAKE_INSTALL_*.
add_subdirectory(ui)

# akcore install goes here (after ui/) so KDE_INSTALL_BINDIR is defined.
install(PROGRAMS ${CMAKE_BINARY_DIR}/akcore
        DESTINATION ${KDE_INSTALL_BINDIR})
