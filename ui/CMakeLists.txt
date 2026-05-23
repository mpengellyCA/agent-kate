# agentkate — the C++/Qt6/KF6 UI, a native KDE application.
find_package(ECM 6.0 NO_MODULE)
if(ECM_FOUND)
    list(APPEND CMAKE_MODULE_PATH ${ECM_MODULE_PATH})
endif()

set(CMAKE_AUTOMOC ON)
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(Qt6 6.6 REQUIRED COMPONENTS Core Gui Widgets Network)
find_package(KF6 6.0 REQUIRED COMPONENTS
    TextEditor          # the Kate editor engine
    SyntaxHighlighting
    XmlGui              # KMainWindow, KHelpMenu
    ConfigWidgets       # KStandardAction
    Config              # KSharedConfig, KConfigGroup (KF6::ConfigCore)
    CoreAddons          # KAboutData, KPluginFactory
    I18n                # i18n()
    Parts               # KParts — embedding the Konsole terminal
    WidgetsAddons       # KMultiTabBar — Kate-style collapsible side/bottom strip
)

add_executable(agentkate
    src/main.cpp
    src/MainWindow.cpp
    src/EditorArea.cpp
    src/DiffView.cpp
    src/ExtensionsDialog.cpp
    src/ProblemsPanel.cpp
    src/ReferencesPanel.cpp
    src/OutlinePanel.cpp
    src/ProjectTree.cpp
    src/TerminalPanel.cpp
    src/SessionBrowserDialog.cpp
    src/SkillsDialog.cpp
    src/AgentRoster.cpp
    src/AgentDock.cpp
    src/AgentPanel.cpp
    src/ipc/CoreClient.cpp
    src/lsp/LspClient.cpp
    src/lsp/LspCompletionModel.cpp
    src/lsp/LspHoverProvider.cpp
    src/lsp/LspManager.cpp
)

target_include_directories(agentkate PRIVATE src)

target_link_libraries(agentkate PRIVATE
    Qt6::Core
    Qt6::Gui
    Qt6::Widgets
    Qt6::Network
    KF6::TextEditor
    KF6::SyntaxHighlighting
    KF6::XmlGui
    KF6::ConfigWidgets
    KF6::ConfigCore
    KF6::CoreAddons
    KF6::I18n
    KF6::Parts
    KF6::WidgetsAddons
)

# Place the UI binary in the build root so it sits next to akcore; the UI
# locates the core relative to its own executable directory.
set_target_properties(agentkate PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR}
)
