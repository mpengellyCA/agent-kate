#include "AgentRoster.h"
#include "AgentCardDelegate.h"
#include "shell/FlowLayout.h"

#include <QAction>
#include <QApplication>
#include <QClipboard>
#include <QDateTime>
#include <QDesktopServices>
#include <QDir>
#include <QEvent>
#include <QFont>
#include <QHBoxLayout>
#include <QIcon>
#include <QHash>
#include <QKeyEvent>
#include <QLabel>
#include <QLineEdit>
#include <QMap>
#include <QMenu>
#include <QPalette>
#include <QResizeEvent>
#include <QSet>
#include <QShortcut>
#include <QSignalBlocker>
#include <QShowEvent>
#include <QTimer>
#include <QToolButton>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QUrl>
#include <QVBoxLayout>

#include <KLocalizedString>

namespace {
// Role layout lives in AgentCardDelegate.h (the delegate paints these). The
// raw title and worktree number are stored separately so the (still-set, for
// accessibility/tooltips) item text can be recomposed when either changes.
using AgentRoles::Number;
using AgentRoles::Tags;
using AgentRoles::Title;

QString composeLabel(int number, const QString &title)
{
    if (number > 0) {
        // Keep the "#N" glyph layout stable; accessible text routes via i18nc.
        return i18nc("@item agent row, '#<number> <title>'", "#%1  %2", number, title);
    }
    return title;
}
} // namespace

AgentRoster::AgentRoster(QWidget *parent)
    : QWidget(parent)
    , m_tree(new QTreeWidget(this))
{
    auto *openButton = new QToolButton(this);
    openButton->setText(i18n("Open Project…"));
    openButton->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    openButton->setIcon(QIcon::fromTheme(QStringLiteral("folder-open")));
    openButton->setToolTip(i18n("Open an existing project folder as a workspace"));
    openButton->setCursor(Qt::PointingHandCursor);
    connect(openButton, &QToolButton::clicked, this, &AgentRoster::openProjectRequested);

    // "+ New Agent" is a tool button with a model-pre-pick dropdown: a plain
    // click starts an agent on the project's default model; the menu lets the
    // user pick a model up front. The model is forwarded into the panel before
    // its first start (agent.start already accepts a model — no new IPC).
    m_newButton = new QToolButton(this);
    m_newButton->setText(i18n("New Agent"));
    m_newButton->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_newButton->setIcon(QIcon::fromTheme(QStringLiteral("list-add")));
    m_newButton->setToolTip(i18n("Start a new agent in the selected project"));
    m_newButton->setPopupMode(QToolButton::MenuButtonPopup);
    m_newButton->setCursor(Qt::PointingHandCursor);
    connect(m_newButton, &QToolButton::clicked, this,
            [this] { emit newAgentRequested(selectedProject()); });

    m_filterEdit = new QLineEdit(this);
    m_filterEdit->setClearButtonEnabled(true);
    m_filterEdit->setPlaceholderText(i18n("Filter agents…"));
    m_filterEdit->addAction(QIcon::fromTheme(QStringLiteral("search")),
                            QLineEdit::LeadingPosition);
    connect(m_filterEdit, &QLineEdit::textChanged, this, &AgentRoster::setFilter);

    m_tree->setHeaderHidden(true);
    m_tree->setIndentation(14);
    m_tree->setContextMenuPolicy(Qt::CustomContextMenu);
    // Agent rows render as tall multi-line cards; project rows stay compact, so
    // rows are no longer a uniform height.
    m_tree->setUniformRowHeights(false);
    m_tree->setItemDelegate(new AgentCardDelegate(m_tree));
    m_tree->installEventFilter(this);

    // Working-badge animation: a single ~10fps timer that repaints only the
    // rows whose agent is Working. Started/stopped by updateWorkingAnimation()
    // so it costs nothing while every agent is idle.
    m_workingTimer = new QTimer(this);
    m_workingTimer->setInterval(100);
    connect(m_workingTimer, &QTimer::timeout, this,
            &AgentRoster::repaintWorkingRows);

    connect(m_tree, &QTreeWidget::currentItemChanged, this,
            [this](QTreeWidgetItem *item, QTreeWidgetItem *previous) {
                // The previous row may now need its marker back: if it is still
                // blocked (AttentionRaw), navigating away from it re-shows the
                // "needs input" marker. Re-derive its display state first.
                if (previous && previous->parent()) {
                    applyAttentionDisplay(previous);
                }
                if (!item) {
                    return;
                }
                if (item->parent()) {
                    const int id = item->data(0, Qt::UserRole).toInt();
                    // Becoming current hides the marker — the user is looking
                    // right at it — but the AttentionRaw truth is preserved.
                    applyAttentionDisplay(item);
                    emit agentActivated(id);
                } else {
                    emit projectFocused(item->data(0, Qt::UserRole).toString());
                }
            });

    connect(m_tree, &QTreeWidget::customContextMenuRequested, this, [this](const QPoint &pos) {
        QTreeWidgetItem *item = m_tree->itemAt(pos);
        if (!item) {
            return;
        }
        QMenu menu(this);
        if (item->parent()) {
            const int id = item->data(0, Qt::UserRole).toInt();
            const bool dormant = item->data(0, AgentRoles::Dormant).toBool();
            QAction *resumeAct = nullptr;
            if (dormant) {
                resumeAct = menu.addAction(
                    QIcon::fromTheme(QStringLiteral("media-playback-start")),
                    i18n("Resume agent"));
                menu.addSeparator();
            }
            QAction *renameAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("document-edit")), i18n("Rename…"));
            QAction *forkAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Fork…"));
            forkAct->setToolTip(
                i18n("Continue this conversation as a new agent on a different model."));
            menu.addSeparator();
            const int number = item->data(0, AgentRoles::Number).toInt();
            QAction *termAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                i18n("Open Terminal in Worktree"));
            // A worktree exists only once the agent has been assigned a #N and
            // is not dormant; otherwise there is no directory to open.
            termAct->setEnabled(number > 0 && !dormant);
            menu.addSeparator();
            QAction *commitAct = menu.addAction(i18n("Commit changes…"));
            QAction *prAct = menu.addAction(i18n("Create pull request…"));
            QAction *landAct = menu.addAction(i18n("Merge into local main…"));
            QAction *discardAct = menu.addAction(i18n("Discard worktree"));
            menu.addSeparator();

            // Tags submenu: a checkable entry per tag already used anywhere in
            // this agent's project, checked when the agent carries it. Toggling
            // emits add/removeTagRequested; "Edit tags…" opens the full editor.
            QMenu *tagsMenu = menu.addMenu(
                QIcon::fromTheme(QStringLiteral("tag")), i18n("Tags"));
            const QString projectPath =
                item->parent() ? item->parent()->data(0, Qt::UserRole).toString()
                               : QString();
            const QStringList own = item->data(0, Tags).toStringList();
            QSet<QString> ownLower;
            for (const QString &t : own) {
                ownLower.insert(t.toLower());
            }
            QHash<QAction *, QString> tagActions;
            const QStringList all = projectTags(projectPath);
            for (const QString &tag : all) {
                QAction *a = tagsMenu->addAction(tag);
                a->setCheckable(true);
                a->setChecked(ownLower.contains(tag.toLower()));
                tagActions.insert(a, tag);
            }
            if (!all.isEmpty()) {
                tagsMenu->addSeparator();
            }
            QAction *editTagsAct = tagsMenu->addAction(i18n("Edit tags…"));
            menu.addSeparator();

            QAction *closeAct = menu.addAction(i18n("Close agent"));
            QAction *chosen = menu.exec(m_tree->viewport()->mapToGlobal(pos));
            if (!chosen) {
                return;
            }
            if (tagActions.contains(chosen)) {
                const QString tag = tagActions.value(chosen);
                if (chosen->isChecked()) {
                    emit addTagRequested(id, tag);
                } else {
                    emit removeTagRequested(id, tag);
                }
                return;
            }
            if (chosen == editTagsAct) {
                emit editTagsRequested(id);
                return;
            }
            if (chosen == resumeAct) {
                emit resumeRequested(id);
            } else if (chosen == termAct) {
                emit openWorktreeTerminalRequested(id);
            } else if (chosen == renameAct) {
                emit renameRequested(id);
            } else if (chosen == forkAct) {
                emit forkRequested(id);
            } else if (chosen == commitAct) {
                emit commitRequested(id);
            } else if (chosen == prAct) {
                emit prRequested(id);
            } else if (chosen == landAct) {
                emit landRequested(id);
            } else if (chosen == discardAct) {
                emit discardRequested(id);
            } else if (chosen == closeAct) {
                emit closeRequested(id);
            }
        } else {
            const QString path = item->data(0, Qt::UserRole).toString();
            QAction *newAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("list-add")),
                i18n("New agent in this project"));
            QAction *organizeAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("tag")),
                i18n("Auto-organize agents…"));
            menu.addSeparator();
            QAction *termAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("utilities-terminal")),
                i18n("Open Terminal Here"));
            QAction *fmAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("folder-open")),
                i18n("Show in File Manager"));
            QAction *copyAct = menu.addAction(
                QIcon::fromTheme(QStringLiteral("edit-copy")), i18n("Copy Path"));
            menu.addSeparator();
            QAction *expandAct = menu.addAction(i18n("Expand"));
            QAction *collapseAct = menu.addAction(i18n("Collapse"));
            menu.addSeparator();
            QAction *closeOthersAct = menu.addAction(i18n("Close Other Projects"));
            QAction *closeAct = menu.addAction(i18n("Close project"));
            QAction *chosen = menu.exec(m_tree->viewport()->mapToGlobal(pos));
            if (!chosen) {
                return;
            }
            if (chosen == newAct) {
                emit newAgentRequested(path);
            } else if (chosen == organizeAct) {
                emit autoOrganizeRequested(path);
            } else if (chosen == termAct) {
                emit openTerminalRequested(path);
            } else if (chosen == fmAct) {
                openFileManager(path);
            } else if (chosen == copyAct) {
                QApplication::clipboard()->setText(path);
            } else if (chosen == expandAct) {
                item->setExpanded(true);
            } else if (chosen == collapseAct) {
                item->setExpanded(false);
            } else if (chosen == closeOthersAct) {
                emit closeOtherProjectsRequested(path);
            } else if (chosen == closeAct) {
                emit closeProjectRequested(path);
            }
        }
    });

    // A flow layout so the two wide text+icon buttons wrap onto a second line
    // instead of clipping when the roster is dragged narrow.
    auto *buttons = new FlowLayout(0, 6, 6);
    buttons->addWidget(openButton);
    buttons->addWidget(m_newButton);

    // Tag-filter button beside the text filter: a checkable menu of every tag in
    // use. Selecting tags narrows the visible agents (intersection) via
    // applyFilter(), alongside the text filter.
    m_tagFilterButton = new QToolButton(this);
    m_tagFilterButton->setText(i18n("Tags"));
    m_tagFilterButton->setIcon(QIcon::fromTheme(QStringLiteral("tag")));
    m_tagFilterButton->setPopupMode(QToolButton::InstantPopup);
    m_tagFilterButton->setToolButtonStyle(Qt::ToolButtonTextBesideIcon);
    m_tagFilterButton->setToolTip(i18n("Filter agents by tag"));
    m_tagFilterButton->setMenu(new QMenu(m_tagFilterButton));
    rebuildTagFilterMenu();

    auto *filterRow = new QHBoxLayout;
    filterRow->setSpacing(6);
    filterRow->addWidget(m_filterEdit, 1);
    filterRow->addWidget(m_tagFilterButton);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(8, 8, 8, 8);
    layout->setSpacing(8);
    layout->addLayout(buttons);
    layout->addLayout(filterRow);
    layout->addWidget(m_tree, 1);

    // Empty-state hint, centred over the tree viewport when no projects exist.
    m_emptyHint = new QLabel(i18n("No projects open.\nUse “Open Project…” to begin."),
                             m_tree->viewport());
    m_emptyHint->setAlignment(Qt::AlignCenter);
    m_emptyHint->setWordWrap(true);
    // Draw the hint in the muted placeholder tone. Bind the foreground *role*
    // (resolved against the live palette at paint time) rather than baking a
    // colour into the palette, so it tracks a runtime Breeze light/dark switch.
    m_emptyHint->setForegroundRole(QPalette::PlaceholderText);
    m_emptyHint->setAttribute(Qt::WA_TransparentForMouseEvents);
    updateEmptyState();

    // Ctrl+F focuses the filter box from anywhere in the roster.
    auto *findShortcut = new QShortcut(QKeySequence::Find, this);
    findShortcut->setContext(Qt::WidgetWithChildrenShortcut);
    connect(findShortcut, &QShortcut::activated, this, [this] {
        m_filterEdit->setFocus();
        m_filterEdit->selectAll();
    });
}

void AgentRoster::openFileManager(const QString &path) const
{
    if (path.isEmpty()) {
        return;
    }
    // Desktop default handler for the folder — avoids pulling KIO::highlight
    // semantics; the folder opening is enough to "show" it.
    QDesktopServices::openUrl(QUrl::fromLocalFile(path));
}

void AgentRoster::setEngineChoices(const QList<EngineChoice> &choices)
{
    // Same value-unchanged guard as the other setters: an identical choice list
    // would rebuild an identical dropdown menu, so skip the teardown/repopulate
    // (EngineChoice compares field-wise).
    if (choices == m_engineChoices) {
        return;
    }
    m_engineChoices = choices;
    // Tear down any previous menu first so every path (including the empty one)
    // replaces it without leaking. setMenu(nullptr) detaches but does not delete
    // the old QMenu, so do it explicitly.
    if (QMenu *old = m_newButton->menu()) {
        m_newButton->setMenu(nullptr);
        old->deleteLater();
    }
    if (choices.isEmpty()) {
        return;
    }
    auto *menu = new QMenu(m_newButton);
    for (const EngineChoice &c : choices) {
        if (c.header) {
            menu->addSection(c.label); // non-clickable engine section title
            continue;
        }
        QAction *act = menu->addAction(c.label);
        const QString backend = c.backend;
        const QString model = c.model;
        const QString ensemble = c.ensemble;
        const bool manage = c.manage;
        connect(act, &QAction::triggered, this, [this, backend, model, ensemble, manage] {
            if (manage) {
                emit manageEnsemblesRequested();
            } else if (!ensemble.isEmpty()) {
                emit applyEnsembleRequested(selectedProject(), ensemble);
            } else {
                emit newAgentWithEngineRequested(selectedProject(), backend, model);
            }
        });
    }
    m_newButton->setMenu(menu);
}

void AgentRoster::addProject(const QString &path, const QString &name)
{
    if (projectItem(path)) {
        return;
    }
    auto *item = new QTreeWidgetItem(m_tree);
    item->setText(0, name);
    item->setData(0, Qt::UserRole, path);
    item->setToolTip(0, path);
    QFont font = item->font(0);
    font.setBold(true);
    item->setFont(0, font);
    item->setExpanded(true);
    updateEmptyState();
}

void AgentRoster::addAgent(const QString &projectPath, int agentId, const QString &title)
{
    QTreeWidgetItem *project = projectItem(projectPath);
    if (!project) {
        return;
    }
    auto *item = new QTreeWidgetItem(project);
    item->setData(0, Qt::UserRole, agentId);
    item->setData(0, Title, title);
    item->setData(0, Number, 0);
    item->setData(0, AgentRoles::StatusRole,
                  int(AgentRoles::AgentStatus::Idle));
    item->setText(0, composeLabel(0, title));
    project->setExpanded(true);
}

void AgentRoster::setAgentTitle(int agentId, const QString &title)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    // The live, derived-title path must not clobber a name the user set.
    if (item->data(0, AgentRoles::Pinned).toBool()) {
        return;
    }
    item->setData(0, Title, title);
    item->setText(0, composeLabel(item->data(0, Number).toInt(), title));
}

void AgentRoster::setAgentTitlePinned(int agentId, const QString &title)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    item->setData(0, AgentRoles::Pinned, true);
    item->setData(0, Title, title);
    item->setText(0, composeLabel(item->data(0, Number).toInt(), title));
}

void AgentRoster::restoreAgentTitleUnpinned(int agentId, const QString &title)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    item->setData(0, AgentRoles::Pinned, false);
    item->setData(0, Title, title);
    item->setText(0, composeLabel(item->data(0, Number).toInt(), title));
}

bool AgentRoster::isAgentTitlePinned(int agentId) const
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        return item->data(0, AgentRoles::Pinned).toBool();
    }
    return false;
}

QString AgentRoster::agentTitle(int agentId) const
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        return item->data(0, Title).toString();
    }
    return QString();
}

void AgentRoster::setAgentNumber(int agentId, int number)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    if (item->data(0, Number).toInt() == number) {
        return;
    }
    item->setData(0, Number, number);
    item->setText(0, composeLabel(number, item->data(0, Title).toString()));
}

void AgentRoster::setAgentTags(int agentId, const QStringList &tags)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    // Same guard shape as setAgentNumber/setAgentAttention: an identical tag set
    // means the chip line, the filter menu and the filter result are all already
    // correct, so skip the (expensive) full re-measure and menu rebuild.
    if (item->data(0, Tags).toStringList() == tags) {
        return;
    }
    item->setData(0, Tags, tags);
    // A chip line appears/disappears, so the row's sizeHint changed — force the
    // view to re-measure rows (uniform row heights are already off, so the
    // delegate's per-row sizeHint is honoured).
    m_tree->doItemsLayout();
    rebuildTagFilterMenu();
    applyFilter();
}

QStringList AgentRoster::agentTags(int agentId) const
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        return item->data(0, Tags).toStringList();
    }
    return {};
}

void AgentRoster::setAgentStatus(int agentId, int status)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        if (item->data(0, AgentRoles::StatusRole).toInt() == status) {
            return;
        }
        item->setData(0, AgentRoles::StatusRole, status);
        updateWorkingAnimation();
    }
}

// updateWorkingAnimation runs the ~10fps repaint timer only while at least one
// agent is Working, so idle rosters stay perfectly still.
void AgentRoster::updateWorkingAnimation()
{
    bool anyWorking = false;
    for (int i = 0; i < m_tree->topLevelItemCount() && !anyWorking; ++i) {
        const QList<QTreeWidgetItem *> rows = agentRows(m_tree->topLevelItem(i));
        for (QTreeWidgetItem *agent : rows) {
            if (AgentRoles::AgentStatus(agent->data(0, AgentRoles::StatusRole).toInt())
                == AgentRoles::AgentStatus::Working) {
                anyWorking = true;
                break;
            }
        }
    }
    if (anyWorking && !m_workingTimer->isActive()) {
        m_workingTimer->start();
    } else if (!anyWorking && m_workingTimer->isActive()) {
        m_workingTimer->stop();
    }
}

// repaintWorkingRows invalidates just the Working rows each tick so the badge
// arc sweeps without repainting the whole (potentially long) roster.
void AgentRoster::repaintWorkingRows()
{
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        const QList<QTreeWidgetItem *> rows = agentRows(m_tree->topLevelItem(i));
        for (QTreeWidgetItem *agent : rows) {
            if (agent->isHidden()) {
                continue;
            }
            if (AgentRoles::AgentStatus(agent->data(0, AgentRoles::StatusRole).toInt())
                == AgentRoles::AgentStatus::Working) {
                const QRect rect = m_tree->visualItemRect(agent);
                if (!rect.isNull()) {
                    m_tree->viewport()->update(rect);
                }
            }
        }
    }
}

void AgentRoster::setAgentSubtitle(int agentId, const QString &subtitle)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        // The composed status detail (branch/cost/tokens) is now the card's
        // tooltip — the card body shows a chat preview instead.
        item->setData(0, AgentRoles::Subtitle, subtitle);
        item->setToolTip(0, subtitle);
    }
}

void AgentRoster::setAgentPreview(int agentId, const QString &preview,
                                  qint64 activityEpoch)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        if (item->data(0, AgentRoles::Preview).toString() == preview) {
            return;
        }
        item->setData(0, AgentRoles::Preview, preview);
        // Stamp the card's "last activity" time so its relative label ("2m ago")
        // tracks the latest exchange. activityEpoch == 0 means "now" (a live
        // message); > 0 is an explicit event time (used at replay end so restored
        // history keeps its real time); < 0 means leave it unstamped (replayed
        // history with no usable timestamp — the delegate then shows no time
        // rather than a wrong "just now").
        if (activityEpoch >= 0) {
            item->setData(0, AgentRoles::LastActivity,
                          activityEpoch > 0 ? activityEpoch
                                            : QDateTime::currentSecsSinceEpoch());
        }
    }
}

void AgentRoster::setAgentDormant(int agentId, bool dormant)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        item->setData(0, AgentRoles::Dormant, dormant);
        // A worker going dormant (or waking) changes its controller's live tally.
        recomputeWorkerCount(item->parent());
    }
}

void AgentRoster::setAgentAttention(int agentId, bool attention)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    // Record the underlying truth unconditionally, then derive the painted
    // marker from it — suppressed only while this is the row the user is looking
    // at. Keeping the two separate means a still-blocked agent re-shows its
    // marker the instant the user navigates away (see currentItemChanged).
    item->setData(0, AgentRoles::AttentionRaw, attention);
    applyAttentionDisplay(item);
}

// applyAttentionDisplay sets the painted Attention flag from AttentionRaw,
// hiding the marker while the row is current, and rolls the change up into the
// project badge.
void AgentRoster::applyAttentionDisplay(QTreeWidgetItem *item)
{
    if (!item) {
        return;
    }
    const bool raw = item->data(0, AgentRoles::AttentionRaw).toBool();
    const bool show = raw && item != m_tree->currentItem();
    if (item->data(0, AgentRoles::Attention).toBool() == show) {
        return;
    }
    item->setData(0, AgentRoles::Attention, show);
    recomputeProjectBadge(item->parent());
}

void AgentRoster::removeAgent(int agentId)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    QTreeWidgetItem *project = projectOf(item);
    QTreeWidgetItem *parent = item->parent();
    // Closing a controller must not take its workers with it: they are separate
    // agents, still running, and deleting the row here would delete their rows
    // as children. Re-home them onto the project first — an orphaned worker is
    // an ordinary top-level agent (plan 16 P5).
    while (item->childCount() > 0) {
        QTreeWidgetItem *worker = item->takeChild(0);
        if (project) {
            project->addChild(worker);
        } else {
            delete worker; // no project row left to hold it
        }
    }
    delete item;
    recomputeWorkerCount(parent);
    recomputeProjectBadge(project);
    updateWorkingAnimation(); // a removed Working agent may stop the timer
}

void AgentRoster::removeProject(const QString &path)
{
    if (QTreeWidgetItem *item = projectItem(path)) {
        delete item; // also deletes its agent children
    }
    updateEmptyState();
    // Deleting a project drops any Working agents under it — the 10fps repaint
    // timer must re-check whether it still has anything to animate, or it spins
    // forever after the last Working agent is gone.
    updateWorkingAnimation();
}

void AgentRoster::setCurrentAgent(int agentId)
{
    if (QTreeWidgetItem *item = agentItem(agentId)) {
        // Never leave the active agent hidden by an in-flight filter.
        item->setHidden(false);
        if (item->parent()) {
            item->parent()->setHidden(false);
        }
        m_tree->setCurrentItem(item);
    }
}

// --- filtering --------------------------------------------------------------

void AgentRoster::setFilter(const QString &text)
{
    m_filter = text.trimmed();
    applyFilter();
}

void AgentRoster::applyFilter()
{
    const bool textFiltering = !m_filter.isEmpty();
    const bool tagFiltering = !m_tagFilter.isEmpty();
    const bool filtering = textFiltering || tagFiltering;
    QTreeWidgetItem *current = m_tree->currentItem();
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *project = m_tree->topLevelItem(i);
        const QString path = project->data(0, Qt::UserRole).toString();
        // Match the project's real name (folder) and path, never the displayed
        // text(0): recomputeProjectBadge rewrites that to "name  (N)", which
        // would otherwise make the attention-count suffix filterable. A tag
        // filter never matches at the project level — it always narrows to
        // agents carrying the selected tags.
        const QString name = QDir(path).dirName();
        const bool projectTextMatches =
            !textFiltering || name.contains(m_filter, Qt::CaseInsensitive)
            || path.contains(m_filter, Qt::CaseInsensitive);
        int visibleChildren = 0;
        const QList<QTreeWidgetItem *> rows = agentRows(project);
        for (QTreeWidgetItem *agent : rows) {
            const QString title = agent->data(0, Title).toString();
            bool show = !textFiltering || projectTextMatches
                || title.contains(m_filter, Qt::CaseInsensitive);
            if (show && tagFiltering) {
                // Intersection: the agent must carry every selected tag.
                QSet<QString> have;
                const QStringList tags = agent->data(0, Tags).toStringList();
                for (const QString &t : tags) {
                    have.insert(t.toLower());
                }
                for (const QString &want : m_tagFilter) {
                    if (!have.contains(want)) {
                        show = false;
                        break;
                    }
                }
            }
            // Don't yank the active agent's row out from under selection.
            if (!show && agent == current) {
                show = true;
            }
            agent->setHidden(!show);
            if (show) {
                ++visibleChildren;
            }
        }
        // A matching worker must stay reachable: Qt hides a row whose parent is
        // hidden, so un-hide the controllers above every visible row (they are
        // context for the match, not matches themselves).
        for (QTreeWidgetItem *agent : rows) {
            if (agent->isHidden()) {
                continue;
            }
            for (QTreeWidgetItem *up = agent->parent(); up && up->parent();
                 up = up->parent()) {
                if (up->isHidden()) {
                    up->setHidden(false);
                    ++visibleChildren;
                }
            }
        }
        // While filtering, hide projects that match nothing. A pure tag filter
        // keeps projects that still have visible agents; the text filter also
        // keeps name/path matches. Otherwise always show them (even empty ones).
        const bool keepProject = !filtering
            || (textFiltering && projectTextMatches) || visibleChildren > 0;
        project->setHidden(!keepProject);
    }
}

QStringList AgentRoster::projectTags(const QString &projectPath) const
{
    // Map lowercased tag -> first-seen display casing, so the menu shows the
    // user's casing while staying case-insensitive.
    QMap<QString, QString> seen;
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *project = m_tree->topLevelItem(i);
        if (!projectPath.isEmpty()
            && project->data(0, Qt::UserRole).toString() != projectPath) {
            continue;
        }
        const QList<QTreeWidgetItem *> rows = agentRows(project);
        for (QTreeWidgetItem *agent : rows) {
            const QStringList tags = agent->data(0, Tags).toStringList();
            for (const QString &t : tags) {
                const QString key = t.toLower();
                if (!seen.contains(key)) {
                    seen.insert(key, t);
                }
            }
        }
    }
    return QStringList(seen.values()); // QMap iterates keys sorted -> stable order
}

void AgentRoster::rebuildTagFilterMenu()
{
    if (!m_tagFilterButton) {
        return;
    }
    QMenu *menu = m_tagFilterButton->menu();
    if (!menu) {
        return;
    }
    const QStringList all = projectTags(QString()); // every project
    // Drop filter selections for tags that no longer exist anywhere.
    QSet<QString> live;
    live.reserve(all.size());
    for (const QString &t : all) {
        live.insert(t.toLower());
    }
    m_tagFilter.intersect(live);

    // Diff the per-tag checkable actions against the live tag set: remove only
    // departed tags, add only newly-appeared ones. An identical tag set leaves
    // the action objects untouched (no menu->clear()/repopulate churn), which is
    // the common case when a tag *change elsewhere* re-runs this with no net
    // change to the global set.
    QSet<QString> wantKeys = live;
    for (auto it = m_tagFilterActions.begin(); it != m_tagFilterActions.end();) {
        if (!wantKeys.contains(it.key())) {
            // QMenu owns the action; removeAction() detaches it, delete frees it.
            menu->removeAction(it.value());
            delete it.value();
            it = m_tagFilterActions.erase(it);
        } else {
            ++it;
        }
    }

    // Lazily create the persistent trailing structure (placeholder / separator /
    // clear) once; we toggle their visibility instead of recreating them.
    if (!m_tagFilterEmptyAct) {
        m_tagFilterEmptyAct = menu->addAction(i18n("No tags yet"));
        m_tagFilterEmptyAct->setEnabled(false);
        m_tagFilterSeparator = menu->addSeparator();
        m_tagFilterClearAct = menu->addAction(i18n("Clear tag filter"));
        connect(m_tagFilterClearAct, &QAction::triggered, this, [this] {
            m_tagFilter.clear();
            rebuildTagFilterMenu();
            applyFilter();
        });
    }

    // Insert any newly-appeared tags, keeping the menu in `all`'s stable
    // (case-insensitive) order. `all` is already sorted by lowercased key (see
    // projectTags), so a new tag is anchored before the next already-present
    // tag's action — or before the trailing structure when it sorts last.
    for (int i = 0; i < all.size(); ++i) {
        const QString key = all.at(i).toLower();
        if (m_tagFilterActions.contains(key)) {
            continue;
        }
        auto *a = new QAction(all.at(i), menu);
        a->setCheckable(true);
        connect(a, &QAction::toggled, this, [this, key](bool on) {
            if (on) {
                m_tagFilter.insert(key);
            } else {
                m_tagFilter.remove(key);
            }
            // Reflect the count in the button label without rebuilding the
            // open menu (which would invalidate the action being toggled).
            m_tagFilterButton->setText(m_tagFilter.isEmpty()
                                           ? i18n("Tags")
                                           : i18n("Tags (%1)", m_tagFilter.size()));
            applyFilter();
        });
        // Anchor before the first following tag that already has an action;
        // fall back to the separator (start of the trailing structure).
        QAction *anchor = m_tagFilterSeparator;
        for (int k = i + 1; k < all.size(); ++k) {
            auto found = m_tagFilterActions.constFind(all.at(k).toLower());
            if (found != m_tagFilterActions.constEnd()) {
                anchor = found.value();
                break;
            }
        }
        menu->insertAction(anchor, a);
        m_tagFilterActions.insert(key, a);
    }

    // Refresh checked states (filter selections may have been intersected away).
    // QSignalBlocker stops setChecked() from re-emitting toggled() and looping
    // back through applyFilter() while we are only mirroring existing state.
    for (auto it = m_tagFilterActions.begin(); it != m_tagFilterActions.end(); ++it) {
        const bool want = m_tagFilter.contains(it.key());
        if (it.value()->isChecked() != want) {
            QSignalBlocker block(it.value());
            it.value()->setChecked(want);
        }
    }

    // Show the placeholder only when there are no tags; show the separator/clear
    // only when there are. The clear entry tracks whether a filter is active.
    const bool hasTags = !all.isEmpty();
    m_tagFilterEmptyAct->setVisible(!hasTags);
    m_tagFilterSeparator->setVisible(hasTags);
    m_tagFilterClearAct->setVisible(hasTags);
    m_tagFilterClearAct->setEnabled(!m_tagFilter.isEmpty());

    // Reflect whether a tag filter is active in the button label.
    m_tagFilterButton->setText(m_tagFilter.isEmpty()
                                   ? i18n("Tags")
                                   : i18n("Tags (%1)", m_tagFilter.size()));
}

// --- project roll-up --------------------------------------------------------

void AgentRoster::recomputeProjectBadge(QTreeWidgetItem *project)
{
    if (!project || project->parent()) {
        return;
    }
    int attention = 0;
    const QList<QTreeWidgetItem *> rows = agentRows(project);
    for (QTreeWidgetItem *agent : rows) {
        if (agent->data(0, AgentRoles::Attention).toBool()) {
            ++attention;
        }
    }
    // Recompose the project caption with a " (N)" suffix when agents in it need
    // attention. The display name is derived from the project path.
    const QString path = project->data(0, Qt::UserRole).toString();
    QString name = QDir(path).dirName();
    if (name.isEmpty()) {
        name = path;
    }
    if (attention > 0) {
        project->setText(0, i18nc("@item project row with attention count",
                                  "%1  (%2)", name, attention));
    } else {
        project->setText(0, name);
    }
}

// --- empty state ------------------------------------------------------------

void AgentRoster::updateEmptyState()
{
    if (!m_emptyHint) {
        return;
    }
    const bool empty = m_tree->topLevelItemCount() == 0;
    m_emptyHint->setVisible(empty);
    if (empty) {
        m_emptyHint->setGeometry(m_tree->viewport()->rect());
    }
}

void AgentRoster::resizeEvent(QResizeEvent *event)
{
    QWidget::resizeEvent(event);
    if (m_emptyHint && m_emptyHint->isVisible()) {
        m_emptyHint->setGeometry(m_tree->viewport()->rect());
    }
}

void AgentRoster::showEvent(QShowEvent *event)
{
    QWidget::showEvent(event);
    updateEmptyState();
}

// --- keyboard ---------------------------------------------------------------

bool AgentRoster::eventFilter(QObject *watched, QEvent *event)
{
    if (watched == m_tree && event->type() == QEvent::KeyPress) {
        auto *ke = static_cast<QKeyEvent *>(event);
        QTreeWidgetItem *item = m_tree->currentItem();
        if (item && item->parent()) {
            const int id = item->data(0, Qt::UserRole).toInt();
            if (ke->key() == Qt::Key_Delete) {
                emit closeRequested(id);
                return true;
            }
            if (ke->key() == Qt::Key_F2) {
                emit renameRequested(id);
                return true;
            }
            if ((ke->key() == Qt::Key_Return || ke->key() == Qt::Key_Enter)
                && item->data(0, AgentRoles::Dormant).toBool()) {
                emit resumeRequested(id);
                return true;
            }
        }
    }
    return QWidget::eventFilter(watched, event);
}

QTreeWidgetItem *AgentRoster::projectItem(const QString &path) const
{
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        QTreeWidgetItem *item = m_tree->topLevelItem(i);
        if (item->data(0, Qt::UserRole).toString() == path) {
            return item;
        }
    }
    return nullptr;
}

// agentRows flattens a project's agent subtree, depth-first. Worker rows nest
// under their controller (plan 16 P5), so every traversal that used to walk
// project->child(j) goes through this instead — a worker two levels down still
// animates, filters, and counts toward its project's badge.
QList<QTreeWidgetItem *> AgentRoster::agentRows(QTreeWidgetItem *project)
{
    QList<QTreeWidgetItem *> out;
    if (!project) {
        return out;
    }
    QList<QTreeWidgetItem *> stack;
    for (int i = project->childCount() - 1; i >= 0; --i) {
        stack.append(project->child(i));
    }
    while (!stack.isEmpty()) {
        QTreeWidgetItem *item = stack.takeLast();
        out.append(item);
        for (int i = item->childCount() - 1; i >= 0; --i) {
            stack.append(item->child(i));
        }
    }
    return out;
}

QTreeWidgetItem *AgentRoster::projectOf(QTreeWidgetItem *item)
{
    while (item && item->parent()) {
        item = item->parent();
    }
    return item;
}

QTreeWidgetItem *AgentRoster::agentItem(int agentId) const
{
    for (int i = 0; i < m_tree->topLevelItemCount(); ++i) {
        const QList<QTreeWidgetItem *> rows = agentRows(m_tree->topLevelItem(i));
        for (QTreeWidgetItem *agent : rows) {
            if (agent->data(0, Qt::UserRole).toInt() == agentId) {
                return agent;
            }
        }
    }
    return nullptr;
}

void AgentRoster::setAgentParent(int agentId, int parentAgentId)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item) {
        return;
    }
    QTreeWidgetItem *project = projectOf(item);
    QTreeWidgetItem *parent = parentAgentId >= 0 ? agentItem(parentAgentId) : nullptr;
    // A worker may not nest under itself or under its own descendant (a cycle
    // would detach the whole subtree from the tree and leak it).
    for (QTreeWidgetItem *p = parent; p; p = p->parent()) {
        if (p == item) {
            parent = nullptr;
            break;
        }
    }
    // No controller row (never launched from this window, or the human archived
    // it): the worker belongs at the project's top level, not nowhere.
    QTreeWidgetItem *target = parent ? parent : project;
    if (!target || item->parent() == target) {
        return;
    }
    QTreeWidgetItem *oldParent = item->parent();
    const bool wasCurrent = m_tree->currentItem() == item;
    // takeChild detaches without deleting; the item keeps all its roles.
    if (oldParent) {
        oldParent->removeChild(item);
    } else if (project) {
        project->removeChild(item);
    }
    target->addChild(item);
    target->setExpanded(true);
    if (wasCurrent) {
        m_tree->setCurrentItem(item); // reparenting must not steal the selection
    }
    recomputeWorkerCount(oldParent);
    recomputeWorkerCount(target);
}

void AgentRoster::setAgentRole(int agentId, const QString &role)
{
    QTreeWidgetItem *item = agentItem(agentId);
    if (!item || item->data(0, AgentRoles::Role).toString() == role) {
        return;
    }
    item->setData(0, AgentRoles::Role, role);
    recomputeWorkerCount(item);
}

// recomputeWorkerCount tallies a controller's LIVE workers (dormant ones are
// not "running under" anybody), for the ⇄ badge. A no-op for project rows and
// for agents with no children.
void AgentRoster::recomputeWorkerCount(QTreeWidgetItem *controller)
{
    if (!controller || !controller->parent()) {
        return; // project row (or detached) — no worker badge
    }
    int live = 0;
    const QList<QTreeWidgetItem *> rows = agentRows(controller);
    for (QTreeWidgetItem *row : rows) {
        if (!row->data(0, AgentRoles::Dormant).toBool()) {
            ++live;
        }
    }
    if (controller->data(0, AgentRoles::WorkerCount).toInt() != live) {
        controller->setData(0, AgentRoles::WorkerCount, live);
    }
}

QString AgentRoster::selectedProject() const
{
    QTreeWidgetItem *item = m_tree->currentItem();
    if (!item) {
        return QString();
    }
    // Walk to the top-level row: a worker sits two (or more) levels down.
    QTreeWidgetItem *project = projectOf(item);
    return project ? project->data(0, Qt::UserRole).toString() : QString();
}
