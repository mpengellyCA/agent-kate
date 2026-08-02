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
    Notifications       # KNotification — desktop alerts for background agents
    DBusAddons          # KDBusService — single-instance, raise-on-relaunch
    WindowSystem        # activation token handling when a relaunch raises us
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
    src/state/HarnessTraits.cpp
    src/state/DraftStore.cpp
    src/state/EngineAvailability.cpp
    src/state/EnsembleCatalog.cpp
    src/state/RateLimitState.cpp
    src/state/Reactive.cpp
    src/state/SessionProjects.cpp
    src/state/WindowTitle.cpp
    src/theme/ThemeManager.cpp
    src/AppearanceDialog.cpp
    src/CommandPalette.cpp
    src/NewAgentDialog.cpp
    src/EnsembleDialog.cpp
    src/ForkAgentDialog.cpp
    src/MainWindow.cpp
    src/EditorArea.cpp
    src/EditorSession.cpp
    src/DiffView.cpp
    src/ImageView.cpp
    src/KPartView.cpp
    src/CsvView.cpp
    src/XlsxReader.cpp
    src/RichTextView.cpp
    src/MarkdownUtil.cpp
    src/SafeContent.cpp
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
    src/shell/ChipPainter.cpp
    src/cowork/CoworkPanel.cpp
    src/cowork/CapabilityTile.cpp
    src/cowork/CoworkPortal.cpp
    src/cowork/ConsentDialog.cpp
    src/cowork/ControlConsentDialog.cpp
    src/cowork/BrowserLaunch.cpp
    src/notify/AgentNotifier.cpp
    src/CooperationPanel.cpp
    src/AiInspectorPanel.cpp
    src/JobsPanel.cpp
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
    src/ToolInspectorDialog.cpp
    src/WorkflowMonitor.cpp
    src/WorkflowMonitorView.cpp
    src/WorkflowMonitorDialog.cpp
    src/SubAgentTranscriptDialog.cpp
    src/WorktreeDashboard.cpp
    src/WorktreeCardDelegate.cpp
    src/CleanupDialog.cpp
    src/ShutdownDialog.cpp
    src/git/AuthorChipDelegate.cpp
    src/git/BlameController.cpp
    src/git/CommitDetailDialog.cpp
    src/git/CommitDetailPanel.cpp
    src/git/CommitDialog.cpp
    src/git/ConflictDialog.cpp
    src/git/GutterController.cpp
    src/git/LogGraphDelegate.cpp
    src/git/LogModel.cpp
    src/git/LogViewer.cpp
    src/git/PRDialog.cpp
    src/git/RefChipDelegate.cpp
    src/git/WorktreeDiffDialog.cpp
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
    konsole/akkonsole.qrc
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
    KF6::Notifications
    KF6::DBusAddons
    KF6::WindowSystem
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

# The desktop notification catalogue. One [Event/…] per alert kind so each is
# separately configurable — and silenceable — in System Settings ▸ Notifications.
# KNotification finds it by component name, so the basename must stay "agentkate".
install(FILES ${CMAKE_CURRENT_SOURCE_DIR}/agentkate.notifyrc
        DESTINATION ${KDE_INSTALL_KNOTIFYRCDIR})

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

    # Plan 10 phase 2: the virtualized chat transcript model + delegate, plus
    # the stateless formatters that feed them (plan 16 P2's MCP summaries).
    # AgentChatHelpers pulls the harness traits (and their CoreClient probe) in
    # for its model-availability helpers — hence the extra sources.
    add_executable(TranscriptModelTest tests/TranscriptModelTest.cpp
        src/TranscriptModel.cpp src/TranscriptDelegate.cpp
        src/AgentChatHelpers.cpp src/MarkdownUtil.cpp src/AttachmentBuilder.cpp
        src/SafeContent.cpp
        src/state/HarnessTraits.cpp src/ProviderConfig.cpp src/ipc/CoreClient.cpp
        src/theme/ThemeManager.cpp)
    target_include_directories(TranscriptModelTest PRIVATE src)
    target_link_libraries(TranscriptModelTest PRIVATE Qt6::Widgets Qt6::Test
        Qt6::Network KF6::ColorScheme KF6::ConfigCore KF6::I18n
        KF6::SyntaxHighlighting KF6::WidgetsAddons)
    add_test(NAME TranscriptModelTest COMMAND TranscriptModelTest)
    set_tests_properties(TranscriptModelTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F16. QTextDocument::setMarkdown runs md4c with raw HTML ON: it
    # swallows text around angle brackets (e.g. "Reactive<T>") and turns
    # model-authored HTML into live rich text. setMarkdownSafe passes md4c's own
    # MarkdownNoHTML flag, so the property holds by construction; this test pins
    # it, including a scan proving no call site opts out.
    add_executable(MarkdownUtilTest tests/MarkdownUtilTest.cpp src/MarkdownUtil.cpp)
    target_include_directories(MarkdownUtilTest PRIVATE src)
    target_link_libraries(MarkdownUtilTest PRIVATE Qt6::Gui Qt6::Test)
    add_test(NAME MarkdownUtilTest COMMAND MarkdownUtilTest)
    set_tests_properties(MarkdownUtilTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F14/F15: what model-authored content may reach the OS. The link
    # scheme policy and the image-resource allowlist are security gates, so they
    # get a regression test of their own — including the /dev/... special-file
    # case that hung the GUI thread, and the end-to-end render tests proving a
    # markdown image outside every root is never read (MarkdownUtil is linked in
    # so those go through the app's real markdown path).
    add_executable(SafeContentTest tests/SafeContentTest.cpp src/SafeContent.cpp
        src/MarkdownUtil.cpp)
    target_include_directories(SafeContentTest PRIVATE src)
    target_link_libraries(SafeContentTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::I18n KF6::WidgetsAddons)
    add_test(NAME SafeContentTest COMMAND SafeContentTest)
    set_tests_properties(SafeContentTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The Cowork consent prompt is the last thing between a prompt-injected agent
    # and the user's screen, so its copy and its preselected scope are security
    # surface (audit F31/F32/F50): no raw capability keys, one shared vocabulary
    # with the panel, narrowest scope preselected, window titles escaped before
    # they reach a RichText label. CapabilityText.h is header-only.
    add_executable(ConsentDialogTest tests/ConsentDialogTest.cpp
        src/cowork/ConsentDialog.cpp)
    target_include_directories(ConsentDialogTest PRIVATE src)
    target_link_libraries(ConsentDialogTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::I18n KF6::WidgetsAddons)
    add_test(NAME ConsentDialogTest COMMAND ConsentDialogTest)
    set_tests_properties(ConsentDialogTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The sub-agent transcript view's document bounds. A block COUNT does not
    # bound block SIZE, so the dialog's cap needs both — this pins the
    # few-but-huge shape the block cap sails past.
    add_executable(SubAgentTranscriptDialogTest
        tests/SubAgentTranscriptDialogTest.cpp src/SubAgentTranscriptDialog.cpp
        src/AgentChatHelpers.cpp src/MarkdownUtil.cpp src/SafeContent.cpp
        src/theme/ThemeManager.cpp src/state/HarnessTraits.cpp
        src/ProviderConfig.cpp src/ipc/CoreClient.cpp)
    target_include_directories(SubAgentTranscriptDialogTest PRIVATE src)
    target_link_libraries(SubAgentTranscriptDialogTest PRIVATE Qt6::Widgets
        Qt6::Test Qt6::Network KF6::I18n KF6::WidgetsAddons KF6::ConfigCore
        KF6::ConfigGui KF6::ColorScheme KF6::CoreAddons
        KF6::SyntaxHighlighting)
    add_test(NAME SubAgentTranscriptDialogTest COMMAND SubAgentTranscriptDialogTest)
    set_tests_properties(SubAgentTranscriptDialogTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Plan 16 P5: the roster is no longer a two-level tree — workers nest under
    # their controller. Guards the traversals and the two ways a nested agent
    # could be lost (a closed controller, a filter match under a hidden parent).
    # RateLimitState comes along because the roster renders the fleet-level
    # usage-limit strip from it (audit F43) — hence KF6::Notifications, which the
    # coalesced popup needs. Tests switch desktop alerts off, so nothing here
    # ever reaches a notification server.
    add_executable(AgentRosterTest tests/AgentRosterTest.cpp
        src/AgentRoster.cpp src/AgentCardDelegate.cpp src/shell/ChipPainter.cpp
        src/shell/FlowLayout.cpp src/theme/ThemeManager.cpp
        src/state/RateLimitState.cpp)
    target_include_directories(AgentRosterTest PRIVATE src)
    target_link_libraries(AgentRosterTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::ColorScheme KF6::ConfigCore KF6::I18n KF6::CoreAddons
        KF6::Notifications KF6::SyntaxHighlighting)
    add_test(NAME AgentRosterTest COMMAND AgentRosterTest)
    set_tests_properties(AgentRosterTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The per-agent action gates and the labels describing what those actions
    # destroy or open. MainWindow.cpp / AgentDock.cpp / AgentRoster.cpp are
    # compiled by the application target alone, so a guard living in them is
    # invisible to the suite — a verifier deleted every one of them and ctest
    # stayed green. The decision is now a pure function (AgentActions.h) and the
    # roster's context menu is built by a callable that needs no exec(), so this
    # covers the primary path as well as the predicate.
    add_executable(AgentActionsTest tests/AgentActionsTest.cpp
        src/AgentRoster.cpp src/AgentCardDelegate.cpp src/shell/ChipPainter.cpp
        src/shell/FlowLayout.cpp src/theme/ThemeManager.cpp
        src/state/RateLimitState.cpp)
    target_include_directories(AgentActionsTest PRIVATE src)
    target_link_libraries(AgentActionsTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::ColorScheme KF6::ConfigCore KF6::I18n KF6::CoreAddons
        KF6::Notifications KF6::SyntaxHighlighting)
    add_test(NAME AgentActionsTest COMMAND AgentActionsTest)
    set_tests_properties(AgentActionsTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Plan 19: attaching a screenshot is attaching a temp file that gets reaped.
    # Guards the durable copy (survives the origin, never displaces it) and the
    # total attachment budget, whose absence let a send kill the IPC connection.
    add_executable(AttachmentBuilderTest tests/AttachmentBuilderTest.cpp
        src/AttachmentBuilder.cpp)
    target_include_directories(AttachmentBuilderTest PRIVATE src)
    # Qt6::Gui for QImage: raw clipboard/drag images are ingested as pixels.
    target_link_libraries(AttachmentBuilderTest PRIVATE Qt6::Core Qt6::Gui Qt6::Test
        KF6::I18n)
    add_test(NAME AttachmentBuilderTest COMMAND AttachmentBuilderTest)
    set_tests_properties(AttachmentBuilderTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Plan 19: the Jobs panel aggregates work it does not own — agents publish
    # snapshots and it replaces their rows wholesale. Guards the three ways that
    # loses or invents work: one agent's snapshot clobbering another's, rows
    # outliving a closed agent, and a filter hiding a job under its group.
    add_executable(JobsPanelTest tests/JobsPanelTest.cpp
        src/JobsPanel.cpp src/SubAgentTranscriptDialog.cpp src/AgentChatHelpers.cpp
        src/MarkdownUtil.cpp src/theme/ThemeManager.cpp src/state/HarnessTraits.cpp
        src/ipc/CoreClient.cpp src/ProviderConfig.cpp src/SafeContent.cpp)
    target_include_directories(JobsPanelTest PRIVATE src)
    target_link_libraries(JobsPanelTest PRIVATE Qt6::Widgets Qt6::Test Qt6::Network
        KF6::I18n KF6::ConfigCore KF6::CoreAddons KF6::SyntaxHighlighting
        KF6::ColorScheme KF6::WidgetsAddons)
    add_test(NAME JobsPanelTest COMMAND JobsPanelTest)
    set_tests_properties(JobsPanelTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F44/F30/F43 on the panel ITSELF, not on the helpers behind it. The
    # bugs this catches are wiring bugs — an empty-state hint that never hears
    # the isolation picker change, a usage-limit claim never withdrawn — and a
    # test on the free functions cannot see either. It links most of the chat
    # stack because the panel is the chat stack; that is the cost of testing the
    # real widget rather than a stand-in.
    add_executable(AgentPanelTest tests/AgentPanelTest.cpp
        src/AgentPanel.cpp src/AgentCardDelegate.cpp src/AgentChatHelpers.cpp
        src/AttachmentBuilder.cpp src/SafeContent.cpp src/ImageView.cpp
        src/MarkdownUtil.cpp src/ProviderConfig.cpp
        src/SubAgentTranscriptDialog.cpp src/ToolInspectorDialog.cpp
        src/TranscriptDelegate.cpp src/TranscriptModel.cpp
        src/WorkflowMonitor.cpp src/WorkflowMonitorDialog.cpp
        src/WorkflowMonitorView.cpp src/DiffView.cpp src/ipc/CoreClient.cpp
        src/shell/ChipPainter.cpp src/shell/ElidingLabel.cpp
        src/shell/FlowLayout.cpp src/state/DraftStore.cpp
        src/state/EngineAvailability.cpp src/state/HarnessTraits.cpp
        src/state/RateLimitState.cpp src/theme/ThemeManager.cpp)
    target_include_directories(AgentPanelTest PRIVATE src)
    target_link_libraries(AgentPanelTest PRIVATE Qt6::Widgets Qt6::Test
        Qt6::Network KF6::ColorScheme KF6::ConfigCore KF6::ConfigGui
        KF6::CoreAddons KF6::I18n KF6::Notifications KF6::SyntaxHighlighting
        KF6::WidgetsAddons)
    add_test(NAME AgentPanelTest COMMAND AgentPanelTest)
    set_tests_properties(AgentPanelTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Desktop alerts: which agent transitions are worth interrupting the user
    # for. Covers the finish/fail rules, the status+attention dedup (one popup
    # per prompt, arriving in either order), the on-screen suppression rule and
    # a closed agent's late signals.
    add_executable(AgentNotifierTest tests/AgentNotifierTest.cpp
        src/notify/AgentNotifier.cpp)
    target_include_directories(AgentNotifierTest PRIVATE src)
    target_link_libraries(AgentNotifierTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::I18n KF6::Notifications KF6::WindowSystem)
    add_test(NAME AgentNotifierTest COMMAND AgentNotifierTest)
    set_tests_properties(AgentNotifierTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Plan 16 P4: the ensemble round-trip and the editable model combo, the two
    # places the editor could silently write an ensemble that is wrong.
    add_executable(EnsembleDialogTest tests/EnsembleDialogTest.cpp
        src/EnsembleDialog.cpp src/state/EnsembleCatalog.cpp
        src/state/HarnessTraits.cpp src/state/EngineAvailability.cpp
        src/ProviderConfig.cpp src/ipc/CoreClient.cpp)
    target_include_directories(EnsembleDialogTest PRIVATE src)
    target_link_libraries(EnsembleDialogTest PRIVATE Qt6::Widgets Qt6::Test
        Qt6::Network KF6::ConfigCore KF6::I18n KF6::WidgetsAddons)
    add_test(NAME EnsembleDialogTest COMMAND EnsembleDialogTest)
    set_tests_properties(EnsembleDialogTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Plan 17: the editor session keys + restore filter. The regression these
    # guard (a fresh run reopening another project's files) was user-visible.
    add_executable(EditorSessionTest tests/EditorSessionTest.cpp src/EditorSession.cpp)
    target_include_directories(EditorSessionTest PRIVATE src)
    target_link_libraries(EditorSessionTest PRIVATE Qt6::Core Qt6::Test KF6::ConfigCore)
    add_test(NAME EditorSessionTest COMMAND EditorSessionTest)
    set_tests_properties(EditorSessionTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The wrapping FlowLayout — verify it reflows to more rows as width shrinks
    # and that its minimum width is ~one item (so it never pins a panel).
    add_executable(FlowLayoutTest tests/FlowLayoutTest.cpp src/shell/FlowLayout.cpp)
    target_include_directories(FlowLayoutTest PRIVATE src)
    target_link_libraries(FlowLayoutTest PRIVATE Qt6::Widgets Qt6::Test)
    add_test(NAME FlowLayoutTest COMMAND FlowLayoutTest)
    set_tests_properties(FlowLayoutTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The two transport bounds the core also enforces: the socket path handed to
    # akcore must satisfy ipc.assertPrivateDir (or the core refuses to bind and
    # the app never starts), and the inbound frame buffer must be capped (audit
    # F10). Header-only units, so no extra sources.
    add_executable(IpcTransportTest tests/IpcTransportTest.cpp)
    target_include_directories(IpcTransportTest PRIVATE src)
    target_link_libraries(IpcTransportTest PRIVATE Qt6::Core Qt6::Test)
    add_test(NAME IpcTransportTest COMMAND IpcTransportTest)
    set_tests_properties(IpcTransportTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F29: the Worktree Dashboard's discard confirmation. A non-isolated
    # row's dirty count is the user's WHOLE checkout, so the prompt for it must
    # not say "worktree #N", must name the real path, and must disclose that
    # their own uncommitted work goes too. The copy lives in a header-only
    # namespace precisely so it can be asserted on without exec()ing a modal.
    add_executable(WorktreeCopyTest tests/WorktreeCopyTest.cpp)
    target_include_directories(WorktreeCopyTest PRIVATE src)
    target_link_libraries(WorktreeCopyTest PRIVATE Qt6::Widgets Qt6::Test KF6::I18n)
    add_test(NAME WorktreeCopyTest COMMAND WorktreeCopyTest)
    set_tests_properties(WorktreeCopyTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F49/F30: the guided dialog's isolation control — the recommended
    # default must be a mode that also works on a repo with no commits ("auto",
    # not "isolated"), and neither its label nor its tooltip may call a git
    # worktree a sandbox.
    add_executable(NewAgentDialogTest tests/NewAgentDialogTest.cpp
        src/NewAgentDialog.cpp src/ProviderConfig.cpp src/state/EnsembleCatalog.cpp
        src/state/EngineAvailability.cpp
        src/state/HarnessTraits.cpp src/ipc/CoreClient.cpp)
    target_include_directories(NewAgentDialogTest PRIVATE src)
    target_link_libraries(NewAgentDialogTest PRIVATE Qt6::Widgets Qt6::Test
        Qt6::Network KF6::ConfigCore KF6::I18n KF6::WidgetsAddons)
    add_test(NAME NewAgentDialogTest COMMAND NewAgentDialogTest)
    set_tests_properties(NewAgentDialogTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F37: whether the agent CLI the core will spawn actually exists. The
    # probe runs against a real $PATH because its whole value is agreeing with
    # what akcore finds; the fixture builds one.
    add_executable(EngineAvailabilityTest tests/EngineAvailabilityTest.cpp
        src/state/EngineAvailability.cpp src/state/HarnessTraits.cpp
        src/ProviderConfig.cpp src/ipc/CoreClient.cpp)
    target_include_directories(EngineAvailabilityTest PRIVATE src)
    target_link_libraries(EngineAvailabilityTest PRIVATE Qt6::Widgets Qt6::Test
        Qt6::Network KF6::ConfigCore KF6::I18n)
    add_test(NAME EngineAvailabilityTest COMMAND EngineAvailabilityTest)
    set_tests_properties(EngineAvailabilityTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F43: the account-level usage-limit state a rate-limited agent is
    # finally visible through — and plan 28 §Phase 2's input, which is why its
    # resetsAt has to survive as a QDateTime rather than a formatted string.
    add_executable(RateLimitStateTest tests/RateLimitStateTest.cpp
        src/state/RateLimitState.cpp)
    target_include_directories(RateLimitStateTest PRIVATE src)
    target_link_libraries(RateLimitStateTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::I18n KF6::Notifications)
    add_test(NAME RateLimitStateTest COMMAND RateLimitStateTest)
    set_tests_properties(RateLimitStateTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F47: the open-project SET, which relaunch used to throw away.
    add_executable(SessionProjectsTest tests/SessionProjectsTest.cpp
        src/state/SessionProjects.cpp)
    target_include_directories(SessionProjectsTest PRIVATE src)
    target_link_libraries(SessionProjectsTest PRIVATE Qt6::Core Qt6::Test
        KF6::ConfigCore)
    add_test(NAME SessionProjectsTest COMMAND SessionProjectsTest)
    set_tests_properties(SessionProjectsTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F46: a provider preset with no key is a choice that can only abort.
    # Built without AK_HAVE_KWALLET, so this covers the env-var path — which is
    # also the one that must be consulted first (a label must never provoke a
    # KWallet unlock prompt).
    add_executable(ProviderConfigTest tests/ProviderConfigTest.cpp
        src/ProviderConfig.cpp)
    target_include_directories(ProviderConfigTest PRIVATE src)
    target_link_libraries(ProviderConfigTest PRIVATE Qt6::Core Qt6::Test
        KF6::ConfigCore KF6::I18n)
    add_test(NAME ProviderConfigTest COMMAND ProviderConfigTest)
    set_tests_properties(ProviderConfigTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # The window title has two independent inputs (active project, agents
    # waiting) and used to have two writers, so one erased the other.
    add_executable(WindowTitleTest tests/WindowTitleTest.cpp
        src/state/WindowTitle.cpp)
    target_include_directories(WindowTitleTest PRIVATE src)
    target_link_libraries(WindowTitleTest PRIVATE Qt6::Core Qt6::Test KF6::I18n)
    add_test(NAME WindowTitleTest COMMAND WindowTitleTest)
    set_tests_properties(WindowTitleTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F50: a closed-with-draft agent used to leak a config entry forever.
    add_executable(DraftStoreTest tests/DraftStoreTest.cpp
        src/state/DraftStore.cpp)
    target_include_directories(DraftStoreTest PRIVATE src)
    target_link_libraries(DraftStoreTest PRIVATE Qt6::Core Qt6::Test
        KF6::ConfigCore)
    add_test(NAME DraftStoreTest COMMAND DraftStoreTest)
    set_tests_properties(DraftStoreTest PROPERTIES
        ENVIRONMENT "QT_QPA_PLATFORM=offscreen")

    # Audit F47/F50: the welcome screen offers the whole previous session, and
    # its recents list has an empty state like every other list in the app.
    add_executable(WelcomeDialogTest tests/WelcomeDialogTest.cpp
        src/WelcomeDialog.cpp src/RecentProjects.cpp src/state/SessionProjects.cpp)
    target_include_directories(WelcomeDialogTest PRIVATE src)
    target_link_libraries(WelcomeDialogTest PRIVATE Qt6::Widgets Qt6::Test
        KF6::ConfigCore KF6::I18n KF6::KIOWidgets)
    add_test(NAME WelcomeDialogTest COMMAND WelcomeDialogTest)
    set_tests_properties(WelcomeDialogTest PROPERTIES
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
    src/NewAgentDialog.cpp
    src/DiffView.cpp
    src/shell/FlowLayout.cpp
    themes/aktheme.qrc)
target_include_directories(akwidgets-preview PRIVATE src)
target_link_libraries(akwidgets-preview PRIVATE
    Qt6::Widgets KF6::I18n KF6::ColorScheme KF6::ConfigCore KF6::SyntaxHighlighting)
set_target_properties(akwidgets-preview PROPERTIES
    RUNTIME_OUTPUT_DIRECTORY ${CMAKE_BINARY_DIR})
