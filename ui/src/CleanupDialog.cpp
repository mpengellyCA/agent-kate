// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "CleanupDialog.h"
#include "ipc/CoreClient.h"

#include <KColorScheme>
#include <KLocalizedString>
#include <KMessageBox>

#include <QApplication>
#include <QCheckBox>
#include <QDialogButtonBox>
#include <QHeaderView>
#include <QIcon>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QLabel>
#include <QPainter>
#include <QProgressDialog>
#include <QPushButton>
#include <QTableView>
#include <QVBoxLayout>

namespace {

// stateColor maps a candidate state to the matching KColorScheme foreground
// role, so badges use the user's native Breeze palette — never hard-coded RGB.
QColor stateColor(const QString &state)
{
    KColorScheme scheme(QPalette::Active, KColorScheme::View);
    if (state == QLatin1String("safe")) {
        return scheme.foreground(KColorScheme::PositiveText).color();
    }
    if (state == QLatin1String("review")) {
        return scheme.foreground(KColorScheme::NeutralText).color();
    }
    if (state == QLatin1String("blocked")) {
        return scheme.foreground(KColorScheme::NegativeText).color();
    }
    if (state == QLatin1String("recordOnly")) {
        // Benign: archives the agent session, touches no files. Normal text.
        return scheme.foreground(KColorScheme::NormalText).color();
    }
    // orphaned (and anything unknown) — muted/inactive.
    return scheme.foreground(KColorScheme::InactiveText).color();
}

QString stateLabel(const QString &state)
{
    if (state == QLatin1String("safe")) {
        return i18n("Safe");
    }
    if (state == QLatin1String("review")) {
        return i18n("Review");
    }
    if (state == QLatin1String("blocked")) {
        return i18n("Blocked");
    }
    if (state == QLatin1String("orphaned")) {
        return i18n("Orphaned");
    }
    if (state == QLatin1String("recordOnly")) {
        // Direct-workspace agents have no worktree; if listed here they aren't
        // running. "Inactive" reflects that, rather than the kind of cleanup.
        return i18n("Inactive");
    }
    return state;
}

// blockerText / warningText translate the core's stable codes into the human
// phrasing shown in tooltips and the loss-confirmation list.
QString blockerText(const QString &code)
{
    if (code == QLatin1String("running")) {
        return i18n("the agent is still running");
    }
    if (code == QLatin1String("notIsolated")) {
        return i18n("not an isolated worktree (the main workspace)");
    }
    if (code == QLatin1String("detachedOrNoBranch")) {
        return i18n("detached HEAD / no branch");
    }
    if (code == QLatin1String("snapshotError")) {
        return i18n("its git state could not be read");
    }
    return code;
}

QString warningText(const CleanupCandidate &c, const QString &code)
{
    if (code == QLatin1String("unmerged")) {
        return i18np("%1 unmerged commit", "%1 unmerged commits", c.ahead);
    }
    if (code == QLatin1String("dirty")) {
        return i18np("%1 uncommitted change", "%1 uncommitted changes", c.dirtyCount);
    }
    if (code == QLatin1String("unpushed")) {
        if (c.unpushedCommits > 0) {
            return i18np("%1 unpushed commit", "%1 unpushed commits", c.unpushedCommits);
        }
        return i18n("never pushed");
    }
    if (code == QLatin1String("hasStash")) {
        return i18np("%1 stash entry", "%1 stash entries", c.stashCount);
    }
    if (code == QLatin1String("branchGoneOnRemote")) {
        return i18n("branch gone on remote");
    }
    return code;
}

// lossSummary describes, in one human sentence, what removing c destroys.
// Used in the confirmation dialog so the user sees EXACTLY what is at stake.
QString lossSummary(const CleanupCandidate &c)
{
    QStringList parts;
    for (const QString &w : c.warnings) {
        parts << warningText(c, w);
    }
    const QString agent = c.number > 0 ? i18n("Agent #%1", c.number) : c.branch;
    if (parts.isEmpty()) {
        return i18n("%1 will be removed.", agent);
    }
    return i18n("%1 has %2 that will be permanently lost.", agent, parts.join(i18n(" and ")));
}

} // namespace

// ---------------------------------------------------------------------------
// CleanupModel
// ---------------------------------------------------------------------------

CleanupModel::CleanupModel(QObject *parent)
    : QAbstractTableModel(parent)
{
}

int CleanupModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_rows.size();
}

int CleanupModel::columnCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : ColCount;
}

QVariant CleanupModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || index.row() < 0 || index.row() >= m_rows.size()) {
        return {};
    }
    const CleanupCandidate &c = m_rows.at(index.row());

    if (role == Qt::CheckStateRole && index.column() == ColCheck) {
        return c.checked ? Qt::Checked : Qt::Unchecked;
    }
    if (role == Qt::DisplayRole) {
        switch (index.column()) {
        case ColAgent:
            return c.number > 0 ? i18n("Agent #%1", c.number) : c.threadId.left(8);
        case ColBranch:
            if (!c.branch.isEmpty()) {
                return c.branch;
            }
            // A direct-workspace agent with no live snapshot has no branch of
            // its own — it shares the checkout's. Label it as such, not detached.
            return c.state == QLatin1String("recordOnly") ? i18n("workspace")
                                                          : i18n("(detached)");
        case ColStatus:
            return stateLabel(c.state);
        case ColRecommendation:
            return c.recommendation.isEmpty() ? QStringLiteral("—") : c.recommendation;
        }
    }
    // Status colour is fetched by the delegate via the state string.
    if (role == Qt::UserRole && index.column() == ColStatus) {
        return c.state;
    }
    if (role == Qt::ToolTipRole) {
        QStringList lines;
        if (!c.title.isEmpty()) {
            lines << c.title;
        }
        if (!c.blockers.isEmpty()) {
            QStringList b;
            for (const QString &code : c.blockers) {
                b << blockerText(code);
            }
            lines << i18n("Blocked: %1", b.join(QStringLiteral(", ")));
        }
        if (!c.warnings.isEmpty()) {
            QStringList w;
            for (const QString &code : c.warnings) {
                w << warningText(c, code);
            }
            lines << i18n("Will be lost: %1", w.join(QStringLiteral(", ")));
        }
        if (c.state == QLatin1String("orphaned")) {
            lines << i18n("Directory is gone; removal only prunes git bookkeeping.");
        }
        if (c.state == QLatin1String("recordOnly")) {
            lines << i18n("No worktree — removal archives the agent's session "
                          "(reversible). Your checkout is left untouched.");
        }
        if (!c.reason.isEmpty()) {
            lines << i18n("AI: %1", c.reason);
        }
        if (!c.path.isEmpty()) {
            lines << c.path;
        }
        return lines.join(QStringLiteral("\n"));
    }
    return {};
}

bool CleanupModel::setData(const QModelIndex &index, const QVariant &value, int role)
{
    if (role != Qt::CheckStateRole || index.column() != ColCheck) {
        return false;
    }
    if (index.row() < 0 || index.row() >= m_rows.size()) {
        return false;
    }
    CleanupCandidate &c = m_rows[index.row()];
    if (!c.removable) {
        return false; // blocked rows can never be checked
    }
    c.checked = value.toInt() == Qt::Checked;
    emit dataChanged(index, index, {Qt::CheckStateRole});
    return true;
}

Qt::ItemFlags CleanupModel::flags(const QModelIndex &index) const
{
    if (!index.isValid()) {
        return Qt::NoItemFlags;
    }
    Qt::ItemFlags f = Qt::ItemIsEnabled | Qt::ItemIsSelectable;
    if (index.column() == ColCheck) {
        const CleanupCandidate &c = m_rows.at(index.row());
        // The checkbox is interactive only for removable rows; blocked rows
        // are shown but not checkable, so destruction can never be requested.
        if (c.removable) {
            f |= Qt::ItemIsUserCheckable;
        } else {
            f &= ~Qt::ItemIsEnabled;
        }
    }
    return f;
}

QVariant CleanupModel::headerData(int section, Qt::Orientation orientation, int role) const
{
    if (role != Qt::DisplayRole || orientation != Qt::Horizontal) {
        return {};
    }
    switch (section) {
    case ColCheck:
        return QString();
    case ColAgent:
        return i18n("Agent");
    case ColBranch:
        return i18n("Branch");
    case ColStatus:
        return i18n("Status");
    case ColRecommendation:
        return i18n("Recommendation");
    }
    return {};
}

void CleanupModel::setCandidates(QList<CleanupCandidate> cands)
{
    beginResetModel();
    m_rows = std::move(cands);
    // Pre-check only safe and orphaned rows — never review (those need a
    // deliberate, informed choice). Record-only workspace agents are pre-checked
    // only once dormant for 48h, so the active agent is never auto-swept.
    const QDateTime staleBefore = QDateTime::currentDateTime().addSecs(-48 * 3600);
    for (CleanupCandidate &c : m_rows) {
        c.checked = c.removable
            && (c.state == QLatin1String("safe") || c.state == QLatin1String("orphaned")
                || (c.state == QLatin1String("recordOnly") && c.lastActivity.isValid()
                    && c.lastActivity < staleBefore));
    }
    endResetModel();
}

QList<CleanupCandidate> CleanupModel::checkedCandidates() const
{
    QList<CleanupCandidate> out;
    for (const CleanupCandidate &c : m_rows) {
        if (c.checked && c.removable) {
            out << c;
        }
    }
    return out;
}

// ---------------------------------------------------------------------------
// CleanupBadgeDelegate
// ---------------------------------------------------------------------------

void CleanupBadgeDelegate::paint(QPainter *painter, const QStyleOptionViewItem &option,
                                 const QModelIndex &index) const
{
    QStyleOptionViewItem opt(option);
    initStyleOption(&opt, index);

    const QString state = index.data(Qt::UserRole).toString();
    const QString label = index.data(Qt::DisplayRole).toString();
    const QColor color = stateColor(state);

    painter->save();
    if (opt.state & QStyle::State_Selected) {
        painter->fillRect(opt.rect, opt.palette.highlight());
    }
    // A small filled dot in the state colour, then the label text.
    const int r = 8;
    QRect dot(opt.rect.left() + 6, opt.rect.center().y() - r / 2, r, r);
    painter->setRenderHint(QPainter::Antialiasing, true);
    painter->setBrush(color);
    painter->setPen(Qt::NoPen);
    painter->drawEllipse(dot);

    QRect textRect = opt.rect.adjusted(6 + r + 6, 0, -4, 0);
    painter->setPen(color);
    painter->drawText(textRect, Qt::AlignVCenter | Qt::AlignLeft, label);
    painter->restore();
}

// ---------------------------------------------------------------------------
// CleanupDialog
// ---------------------------------------------------------------------------

CleanupDialog::CleanupDialog(CoreClient *core, const QString &project, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_project(project)
    , m_view(new QTableView(this))
    , m_model(new CleanupModel(this))
{
    setWindowTitle(i18n("Analyze & Clean Up Worktrees & Agents"));
    resize(720, 420);

    m_view->setModel(m_model);
    m_view->setSelectionBehavior(QAbstractItemView::SelectRows);
    m_view->setSelectionMode(QAbstractItemView::SingleSelection);
    m_view->setShowGrid(false);
    m_view->setAlternatingRowColors(true);
    m_view->verticalHeader()->setVisible(false);
    m_view->horizontalHeader()->setStretchLastSection(true);
    m_view->setItemDelegateForColumn(CleanupModel::ColStatus, new CleanupBadgeDelegate(this));
    m_view->horizontalHeader()->setSectionResizeMode(
        CleanupModel::ColCheck, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        CleanupModel::ColAgent, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        CleanupModel::ColBranch, QHeaderView::ResizeToContents);
    m_view->horizontalHeader()->setSectionResizeMode(
        CleanupModel::ColStatus, QHeaderView::ResizeToContents);

    m_advise = new QCheckBox(i18n("Include AI recommendations (Sonnet)"), this);
    m_status = new QLabel(this);
    m_status->setWordWrap(true);

    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Close, this);
    m_removeBtn = new QPushButton(QIcon::fromTheme(QStringLiteral("edit-delete")),
                                  i18n("Archive && Remove selected"), this);
    buttons->addButton(m_removeBtn, QDialogButtonBox::ActionRole);

    auto *layout = new QVBoxLayout(this);
    layout->addWidget(m_view, 1);
    layout->addWidget(m_advise);
    layout->addWidget(m_status);
    layout->addWidget(buttons);

    connect(buttons, &QDialogButtonBox::rejected, this, &QDialog::reject);
    connect(m_removeBtn, &QPushButton::clicked, this, &CleanupDialog::onRemoveClicked);
    connect(m_advise, &QCheckBox::toggled, this, [this](bool on) { analyze(on); });

    analyze(false);
}

void CleanupDialog::analyze(bool advise)
{
    m_status->setText(i18n("Analyzing worktrees…"));
    m_removeBtn->setEnabled(false);
    if (advise) {
        QApplication::setOverrideCursor(Qt::BusyCursor);
    }
    m_core->call(QStringLiteral("cleanup.analyze"),
                 QJsonObject{{QStringLiteral("project"), m_project},
                             {QStringLiteral("advise"), advise}},
                 [this, advise](const QJsonObject &result, const QJsonObject &error) {
                     if (advise) {
                         QApplication::restoreOverrideCursor();
                     }
                     m_removeBtn->setEnabled(true);
                     if (!error.isEmpty()) {
                         m_status->setText(i18n("Analysis failed: %1",
                             error.value(QStringLiteral("message")).toString()));
                         return;
                     }
                     applyResult(result);
                 });
}

void CleanupDialog::applyResult(const QJsonObject &result)
{
    const QJsonArray arr = result.value(QStringLiteral("candidates")).toArray();
    QList<CleanupCandidate> cands;
    cands.reserve(arr.size());
    int safe = 0, review = 0, blocked = 0, orphaned = 0, recordOnly = 0;
    for (const QJsonValue &v : arr) {
        const QJsonObject o = v.toObject();
        CleanupCandidate c;
        c.threadId = o.value(QStringLiteral("threadId")).toString();
        c.number = o.value(QStringLiteral("number")).toInt();
        c.branch = o.value(QStringLiteral("branch")).toString();
        c.path = o.value(QStringLiteral("path")).toString();
        c.title = o.value(QStringLiteral("title")).toString();
        c.state = o.value(QStringLiteral("state")).toString();
        c.merged = o.value(QStringLiteral("merged")).toBool();
        c.ahead = o.value(QStringLiteral("ahead")).toInt();
        c.dirtyCount = o.value(QStringLiteral("dirtyCount")).toInt();
        c.unpushedCommits = o.value(QStringLiteral("unpushedCommits")).toInt();
        c.stashCount = o.value(QStringLiteral("stashCount")).toInt();
        c.lastActivity = QDateTime::fromString(
            o.value(QStringLiteral("lastActivity")).toString(), Qt::ISODate);
        c.diffStat = o.value(QStringLiteral("diffStat")).toString();
        c.removable = o.value(QStringLiteral("removable")).toBool();
        c.recommendation = o.value(QStringLiteral("recommendation")).toString();
        c.reason = o.value(QStringLiteral("reason")).toString();
        c.error = o.value(QStringLiteral("error")).toString();
        for (const QJsonValue &b : o.value(QStringLiteral("blockers")).toArray()) {
            c.blockers << b.toString();
        }
        for (const QJsonValue &w : o.value(QStringLiteral("warnings")).toArray()) {
            c.warnings << w.toString();
        }
        if (c.state == QLatin1String("safe")) {
            ++safe;
        } else if (c.state == QLatin1String("review")) {
            ++review;
        } else if (c.state == QLatin1String("blocked")) {
            ++blocked;
        } else if (c.state == QLatin1String("orphaned")) {
            ++orphaned;
        } else if (c.state == QLatin1String("recordOnly")) {
            ++recordOnly;
        }
        cands << c;
    }
    m_model->setCandidates(cands);
    m_status->setText(i18n("%1 safe · %2 inactive · %3 to review · %4 orphaned · %5 blocked",
                           safe, recordOnly, review, orphaned, blocked));
}

void CleanupDialog::onRemoveClicked()
{
    const QList<CleanupCandidate> targets = m_model->checkedCandidates();
    if (targets.isEmpty()) {
        m_status->setText(i18n("Select at least one removable worktree."));
        return;
    }

    // If ANY selected row carries warnings, enumerate the losses and require an
    // explicit, Cancel-defaulted confirmation before going anywhere near git.
    QStringList losses;
    for (const CleanupCandidate &c : targets) {
        if (!c.warnings.isEmpty()) {
            losses << lossSummary(c);
        }
    }
    if (!losses.isEmpty()) {
        const KMessageBox::ButtonCode answer = KMessageBox::warningContinueCancelList(
            this,
            i18n("The following worktrees contain work that will be permanently lost. "
                 "This cannot be undone. Continue?"),
            losses,
            i18n("Permanent data loss"),
            KGuiItem(i18n("Archive && Remove"),
                     QIcon::fromTheme(QStringLiteral("edit-delete"))),
            KStandardGuiItem::cancel(),
            QString(),
            KMessageBox::Options(KMessageBox::Notify | KMessageBox::Dangerous));
        if (answer != KMessageBox::Continue) {
            return;
        }
    }

    runRemovals(targets);
}

void CleanupDialog::runRemovals(const QList<CleanupCandidate> &targets)
{
    m_queue = targets;
    m_queueIndex = 0;
    m_removed.clear();
    m_failures.clear();

    m_progress = new QProgressDialog(i18n("Removing worktrees…"), i18n("Cancel"),
                                     0, m_queue.size(), this);
    m_progress->setWindowModality(Qt::WindowModal);
    m_progress->setMinimumDuration(0);
    m_progress->setValue(0);

    removeNext();
}

void CleanupDialog::removeNext()
{
    // Stop early if the user cancelled, or we ran out of work.
    if (m_queueIndex >= m_queue.size()
        || (m_progress != nullptr && m_progress->wasCanceled())) {
        if (m_progress != nullptr) {
            m_progress->setValue(m_queue.size());
            m_progress->deleteLater();
            m_progress = nullptr;
        }
        // Summarise.
        QStringList lines;
        for (const QString &r : m_removed) {
            lines << i18n("Removed %1", r);
        }
        for (const QString &f : m_failures) {
            lines << i18n("Failed: %1", f);
        }
        KMessageBox::informationList(
            this,
            i18n("Cleanup finished: %1 removed, %2 failed.",
                 m_removed.size(), m_failures.size()),
            lines, i18n("Cleanup complete"));
        emit statusMessage(i18n("Cleanup: %1 removed, %2 failed",
                                m_removed.size(), m_failures.size()));
        emit cleaned();
        analyze(m_advise->isChecked()); // refresh the list in-place
        return;
    }

    const CleanupCandidate c = m_queue.at(m_queueIndex);
    if (m_progress != nullptr) {
        m_progress->setValue(m_queueIndex);
        m_progress->setLabelText(c.number > 0
            ? i18n("Removing Agent #%1…", c.number)
            : i18n("Removing %1…", c.branch));
    }

    // confirmDestroy is required only for rows that actually have warnings;
    // safe/orphaned rows pass false. The core re-verifies regardless.
    const bool confirmDestroy = !c.warnings.isEmpty();
    const QString label = c.number > 0 ? i18n("Agent #%1", c.number) : c.branch;

    m_core->call(QStringLiteral("cleanup.archiveAndRemove"),
                 QJsonObject{{QStringLiteral("threadId"), c.threadId},
                             {QStringLiteral("confirmDestroy"), confirmDestroy}},
                 [this, label](const QJsonObject &, const QJsonObject &error) {
                     if (error.isEmpty()) {
                         m_removed << label;
                     } else {
                         m_failures << i18n("%1 (%2)", label,
                             error.value(QStringLiteral("message")).toString());
                     }
                     ++m_queueIndex;
                     removeNext();
                 });
}
