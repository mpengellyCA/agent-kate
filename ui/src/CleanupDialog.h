// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QAbstractTableModel>
#include <QDialog>
#include <QJsonObject>
#include <QList>
#include <QString>
#include <QStringList>
#include <QStyledItemDelegate>

class CoreClient;
class QCheckBox;
class QLabel;
class QProgressDialog;
class QPushButton;
class QTableView;

// CleanupCandidate mirrors the core's gitstatus.CleanupCandidate, flattened for
// the table. State is the authoritative verdict the core re-derives before any
// removal; the dialog only uses it to drive presentation.
struct CleanupCandidate {
    QString threadId;
    int number = 0;
    QString branch;
    QString path;
    QString title;
    QString state; // "safe" | "review" | "blocked" | "orphaned"
    QStringList blockers;
    QStringList warnings;
    bool merged = false;
    int ahead = 0;
    int dirtyCount = 0;
    int unpushedCommits = 0;
    int stashCount = 0;
    QString diffStat;
    bool removable = false;
    QString recommendation; // phase 2; "" in phase 1
    QString reason;         // phase 2
    QString error;

    bool checked = false; // user selection in the dialog
};

// CleanupModel is the table behind CleanupDialog. The leading column is a
// checkbox (disabled for blocked rows); the rest are read-only labels.
class CleanupModel : public QAbstractTableModel
{
    Q_OBJECT
public:
    enum Column {
        ColCheck = 0,
        ColAgent,
        ColBranch,
        ColStatus,
        ColRecommendation,
        ColCount,
    };

    explicit CleanupModel(QObject *parent = nullptr);
    int rowCount(const QModelIndex &parent = {}) const override;
    int columnCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &index, int role = Qt::DisplayRole) const override;
    bool setData(const QModelIndex &index, const QVariant &value, int role) override;
    Qt::ItemFlags flags(const QModelIndex &index) const override;
    QVariant headerData(int section, Qt::Orientation orientation,
                        int role = Qt::DisplayRole) const override;

    void setCandidates(QList<CleanupCandidate> cands);
    const QList<CleanupCandidate> &candidates() const { return m_rows; }
    QList<CleanupCandidate> checkedCandidates() const;

private:
    QList<CleanupCandidate> m_rows;
};

// CleanupBadgeDelegate paints the Status column as a coloured badge using the
// native KColorScheme roles (green safe, amber review, red blocked, grey
// orphaned) — palette only, no custom theming.
class CleanupBadgeDelegate : public QStyledItemDelegate
{
    Q_OBJECT
public:
    using QStyledItemDelegate::QStyledItemDelegate;
    void paint(QPainter *painter, const QStyleOptionViewItem &option,
               const QModelIndex &index) const override;
};

// CleanupDialog drives the safety-critical worktree cleanup flow: it shows the
// core's analysis, lets the user select removable rows, confirms losses, then
// sequences cleanup.archiveAndRemove calls behind a progress dialog.
class CleanupDialog : public QDialog
{
    Q_OBJECT
public:
    CleanupDialog(CoreClient *core, const QString &project, QWidget *parent = nullptr);

Q_SIGNALS:
    // Emitted after the run completes so the dashboard can refresh + toast.
    void statusMessage(const QString &text);
    void cleaned();

private:
    void analyze(bool advise);
    void applyResult(const QJsonObject &result);
    void onRemoveClicked();
    void runRemovals(const QList<CleanupCandidate> &targets);
    void removeNext();

    CoreClient *m_core = nullptr;
    QString m_project;

    QTableView *m_view = nullptr;
    CleanupModel *m_model = nullptr;
    QCheckBox *m_advise = nullptr;
    QLabel *m_status = nullptr;
    QPushButton *m_removeBtn = nullptr;

    // Removal run state.
    QList<CleanupCandidate> m_queue;
    int m_queueIndex = 0;
    QProgressDialog *m_progress = nullptr;
    QStringList m_removed;
    QStringList m_failures;
};
