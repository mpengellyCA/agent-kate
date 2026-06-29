# agentkate — the C++/Qt6/KF6 UI, a native KDE application.
# ECM + KDEInstallDirs are loaded by the top-level CMakeLists.txt so that
# KDE_INSTALL_* are seeded before any module pulls in GNUInstallDirs.

set(CMAKE_AUTOMOC ON)
set(CMAKE_AUTORCC ON)
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(Qt6 6.6 REQUIRED COMPONENTS Core Gui Widgets Network DBus)

# Optional: libpipewire-0.3 powers the SPIKE-1 screencast-stream consumer that keeps a
# stream "consumed" so KWin honours absolute pointer motion (plan 09, true cursor
# control). The build stays green without it — all libpipewire code is guarded by
# AK_HAVE_PIPEWIRE and the feature simply degrades.
find_package(PkgConfig)
if(PkgConfig_FOUND)
    pkg_check_modules(PipeWire IMPORTED_TARGET libpipewire-0.3)
endif()

find_package(KF6 6.0 REQUIRED COMPONENTS
    TextEditor          # the Kate editor engine
    SyntaxHighlighting
    XmlGui              # KMainWindow, KHelpMenu
    ConfigWidgets       # KStandardAction
    Config              # KSharedConfig, KConfigGroup (KF6::ConfigCore)
    Completion          # KHistoryComboBox — search query recall
    CoreAddons          # KAboutData, KPluginFactory
    I18n                # i18n()
    Parts               # KParts — embedding the Konsole terminal
    WidgetsAddons       # KMultiTabBar — Kate-style collapsible side/bottom strip
    ColorScheme         # KColorScheme — native Breeze status-badge colours
    KIO                 # KIO::copy/move/trash, KPropertiesDialog, highlightInFileManager
    Archive             # KZip — native .xlsx (OOXML zip) reading in CsvView
)

# Optional: KF6 Wallet stores third-party API provider keys (plan 11) in the KDE
# secret service. The build stays green without it — ProviderConfig falls back to
# resolving keys from environment variables (all KWallet code is guarded by
# AK_HAVE_KWALLET and the feature simply degrades to env-var-only keys).
find_package(KF6 6.0 QUIET COMPONENTS Wallet)

add_executable(agentkate
    src/main.cpp
    src/state/Reactive.cpp
    src/theme/ThemeManager.cpp
    src/AppearanceDialog.cpp
    src/CommandPalette.cpp
    src/MainWindow.cpp
    src/EditorArea.cpp
    src/DiffView.cpp
    src/ImageView.cpp
    src/KPartView.cpp
    src/CsvView.cpp
    src/XlsxReader.cpp
    src/RichTextView.cpp
    src/MarkdownUtil.cpp
    src/ExtensionsDialog.cpp
    src/ProviderConfig.cpp
    src/ProvidersDialog.cpp
    src/ProblemsPanel.cpp
    src/ReferencesPanel.cpp
    src/OutlinePanel.cpp
    src/ProjectTree.cpp
    src/FileFilterProxyModel.cpp
    src/GitStatusDelegate.cpp
    src/RecentProjects.cpp
    src/shell/SideBar.cpp
    src/shell/ShellLayout.cpp
    src/shell/PanelStack.cpp
    src/shell/StubPanel.cpp
    src/shell/FlowLayout.cpp
    src/shell/ElidingLabel.cpp
    src/cowork/CoworkPanel.cpp
    src/cowork/CoworkPortal.cpp
    src/cowork/ConsentDialog.cpp
    src/cowork/ControlConsentDialog.cpp
    src/cowork/BrowserLaunch.cpp
    src/CooperationPanel.cpp
    src/AiInspectorPanel.cpp
    src/TerminalPanel.cpp
    src/SearchPanel.cpp
    src/SessionBrowserDialog.cpp
    src/SkillsDialog.cpp
    src/WelcomeDialog.cpp
    src/AgentRoster.cpp
    src/AgentCardDelegate.cpp
    src/AgentDock.cpp
    src/TagEditorDialog.cpp
    src/AutoOrganizeDialog.cpp
    src/AgentPanel.cpp
    src/AgentChatHelpers.cpp
    src/AttachmentBuilder.cpp
    src/TranscriptModel.cpp
    src/TranscriptDelegate.cpp
    src/WorktreeDashboard.cpp
    src/CleanupDialog.cpp
    src/ShutdownDialog.cpp
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
    src/lsp/LspActions.cpp
    src/lsp/LspClient.cpp
    src/lsp/LspCompletionModel.cpp
    src/lsp/LspHoverProvider.cpp
    src/lsp/LspManager.cpp
    src/lsp/LspSignatureHelp.cpp
    src/lsp/WorkspaceSymbolDialog.cpp
    icons/agentkate.qrc
    themes/aktheme.qrc
)

target_include_directories(agentkate PRIVATE src)

target_link_libraries(agentkate PRIVATE
    Qt6::Core
    Qt6::Gui
    Qt6::Widgets
    Qt6::Network
    Qt6::DBus
    KF6::TextEditor
    KF6::SyntaxHighlighting
    KF6::XmlGui
    KF6::ConfigWidgets
    KF6::ConfigCore
    KF6::Completion
    KF6::CoreAddons
    KF6::I18n
    KF6::Parts
    KF6::WidgetsAddons
    KF6::ColorScheme
    KF6::KIOCore
    KF6::KIOGui
    KF6::KIOWidgets
    KF6::Archive
)

# SPIKE-1 cursor-control consumer: link libpipewire and flip AK_HAVE_PIPEWIRE only when
# the dev package is present. Absent, CoworkPortal still compiles (all libpipewire code is
# #ifdef AK_HAVE_PIPEWIRE) and the screencast-consumer defense is simply omitted.
if(TARGET PkgConfig::PipeWire)
    target_compile_definitions(agentkate PRIVATE AK_HAVE_PIPEWIRE)
    target_link_libraries(agentkate PRIVATE PkgConfig::PipeWire)
    message(STATUS "agentkate: libpipewire-0.3 found — cowork cursor-control consumer enabled")
else()
    message(STATUS "agentkate: libpipewire-0.3 not found — cowork cursor-control consumer disabled")
endif()

# Provider key storage: link KF6::Wallet and flip AK_HAVE_KWALLET when present.
# Absent, ProviderConfig compiles fine (all KWallet code is #ifdef AK_HAVE_KWALLET)
# and provider keys are resolved from environment variables only.
if(TARGET KF6::Wallet)
    target_compile_definitions(agentkate PRIVATE AK_HAVE_KWALLET)
    target_link_libraries(agentkate PRIVATE KF6::Wallet)
    message(STATUS "agentkate: KF6Wallet found — provider API keys stored in KWallet")
else()
    message(STATUS "agentkate: KF6Wallet not found — provider keys via env vars only")
endif()

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
# Configure the .desktop at build time so Exec carries the absolute install path
# (@KDE_INSTALL_FULL_BINDIR@) — required for KWin to authorize the no-dialog ScreenShot2
# capture used by Cowork (it matches /proc/<pid>/exe against this Exec). See the template.
configure_file(${CMAKE_CURRENT_SOURCE_DIR}/org.kde.agentkate.desktop.in
               ${CMAKE_CURRENT_BINARY_DIR}/org.kde.agentkate.desktop @ONLY)
install(FILES ${CMAKE_CURRENT_BINARY_DIR}/org.kde.agentkate.desktop
        DESTINATION ${KDE_INSTALL_APPDIR})
install(FILES ${CMAKE_CURRENT_SOURCE_DIR}/org.kde.agentkate.metainfo.xml
        DESTINATION ${KDE_INSTALL_METAINFODIR})

# Install the UI binary. akcore lives next to it (installed by the top-level
# CMakeLists) so applicationDirPath() finds the core after `make install`.
install(TARGETS agentkate RUNTIME DESTINATION ${KDE_INSTALL_BINDIR})

# --- Tests -----------------------------------------------------------------
# Focused QtTest coverage for the panel-shell sizing fix (plan 10). Guarded on
# Qt6::Test so release/packaging builds without it still configure cleanly. Runs
# headless under the offscreen QPA platform.
find_package(Qt6 6.6 QUIET COMPONENTS Test)
if(Qt6Test_FOUND)
    enable_testing()
    add_executable(PanelStackTest tests/PanelStackTest.cpp src/shell/PanelStack.cpp)
    target_include_directories(PanelStackTest PRIVATE src)
    target_link_libraries(PanelStackTest PRIVATE Qt6::Widgets Qt6::Test)
    add_test(NAME PanelStackTest COMMAND PanelStackTest)
    set_tests_properties(PanelStackTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Plan 10 phase 2: the virtualized chat transcript model + delegate.
    add_executable(TranscriptModelTest tests/TranscriptModelTest.cpp
        src/TranscriptModel.cpp src/TranscriptDelegate.cpp
        src/theme/ThemeManager.cpp)
    target_include_directories(TranscriptModelTest PRIVATE src)
    target_link_libraries(TranscriptModelTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::ColorScheme KF6::ConfigCore)
    add_test(NAME TranscriptModelTest COMMAND TranscriptModelTest)
    set_tests_properties(TranscriptModelTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Regression guard: QTextDocument::setMarkdown swallows text around raw-HTML
    # angle brackets (e.g. "Reactive<T>"); the neutralizer escapes them first.
    add_executable(MarkdownUtilTest tests/MarkdownUtilTest.cpp src/MarkdownUtil.cpp)
    target_include_directories(MarkdownUtilTest PRIVATE src)
    target_link_libraries(MarkdownUtilTest PRIVATE Qt6::Gui Qt6::Test)
    add_test(NAME MarkdownUtilTest COMMAND MarkdownUtilTest)
    set_tests_properties(MarkdownUtilTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The wrapping FlowLayout — verify it reflows to more rows as width shrinks
    # and that its minimum width is ~one item (so it never pins a panel).
    add_executable(FlowLayoutTest tests/FlowLayoutTest.cpp src/shell/FlowLayout.cpp)
    target_include_directories(FlowLayoutTest PRIVATE src)
    target_link_libraries(FlowLayoutTest PRIVATE Qt6::Widgets Qt6::Test)
    add_test(NAME FlowLayoutTest COMMAND FlowLayoutTest)
    set_tests_properties(FlowLayoutTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")
endif()

# Dev-only visual harness for the new Appearance dialog + Command Palette. Not a
# ctest and not installed — it shows the widgets under a chosen Agent Kate theme
# so they can be eyeballed/screenshotted in isolation. Build tree only.
add_executable(akwidgets-preview EXCLUDE_FROM_ALL
    tests/WidgetsPreview.cpp
    src/theme/ThemeManager.cpp
    src/AppearanceDialog.cpp
    src/CommandPalette.cpp
    src/DiffView.cpp
    src/shell/FlowLayout.cpp
    themes/aktheme.qrc)
target_include_directories(akwidgets-preview PRIVATE src)
target_link_libraries(akwidgets-preview PRIVATE
    Qt6::Widgets KF6::I18n KF6::ColorScheme KF6::ConfigCore KF6::SyntaxHighlighting)
set_target_properties(akwidgets-preview PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR})
