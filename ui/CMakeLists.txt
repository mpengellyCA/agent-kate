# agentkate — the C++/Qt6/KF6 UI, a native KDE application.
# ECM + KDEInstallDirs are loaded by the top-level CMakeLists.txt so that
# KDE_INSTALL_* are seeded before any module pulls in GNUInstallDirs.

set(CMAKE_AUTOMOC ON)
set(CMAKE_AUTORCC ON)
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
    src/RecentProjects.cpp
    src/shell/SideBar.cpp
    src/shell/ShellLayout.cpp
    src/TerminalPanel.cpp
    src/SessionBrowserDialog.cpp
    src/SkillsDialog.cpp
    src/WelcomeDialog.cpp
    src/AgentRoster.cpp
    src/AgentDock.cpp
    src/AgentPanel.cpp
    src/WorktreeDashboard.cpp
    src/git/BlameController.cpp
    src/git/CommitDetailPanel.cpp
    src/git/CommitDialog.cpp
    src/git/ConflictDialog.cpp
    src/git/GutterController.cpp
    src/git/LogGraphDelegate.cpp
    src/git/LogModel.cpp
    src/git/LogViewer.cpp
    src/git/PRDialog.cpp
    src/git/RefChipDelegate.cpp
    src/ipc/CoreClient.cpp
    src/lsp/LspClient.cpp
    src/lsp/LspCompletionModel.cpp
    src/lsp/LspHoverProvider.cpp
    src/lsp/LspManager.cpp
    icons/agentkate.qrc
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

# Install hicolor application icons and the .desktop entry so that KDE/Plasma
# (taskbar, KRunner, Kickoff, window decorations) shows the Agent Kate logo.
ecm_install_icons(
    ICONS
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/16-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/22-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/32-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/48-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/64-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/128-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/256-apps-agentkate.png
        ${CMAKE_CURRENT_SOURCE_DIR}/icons/512-apps-agentkate.png
    DESTINATION ${KDE_INSTALL_ICONDIR}
    THEME hicolor
)
install(FILES ${CMAKE_CURRENT_SOURCE_DIR}/org.kde.agentkate.desktop
        DESTINATION ${KDE_INSTALL_APPDIR})
install(FILES ${CMAKE_CURRENT_SOURCE_DIR}/org.kde.agentkate.metainfo.xml
        DESTINATION ${KDE_INSTALL_METAINFODIR})

# Install the UI binary. akcore lives next to it (installed by the top-level
# CMakeLists) so applicationDirPath() finds the core after `make install`.
install(TARGETS agentkate RUNTIME DESTINATION ${KDE_INSTALL_BINDIR})
