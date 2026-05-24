# AgentKate superbuild: builds the C++/Qt UI and the Go orchestration core.
cmake_minimum_required(VERSION 3.20)
project(AgentKate VERSION 0.1.0 LANGUAGES CXX)

# Emit build/compile_commands.json so clangd (AgentKate's own LSP) resolves the
# Qt6/KF6 include paths and gives accurate diagnostics for this project.
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

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

include(GNUInstallDirs)
install(PROGRAMS ${CMAKE_BINARY_DIR}/akcore
        DESTINATION ${CMAKE_INSTALL_BINDIR})

# --- C++/Qt UI (agentkate) -------------------------------------------------
add_subdirectory(ui)
