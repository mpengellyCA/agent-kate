// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QJsonArray>
#include <QObject>
#include <QPointer>
#include <QString>

namespace KTextEditor {
class Document;
class View;
}
class CoreClient;
class QEvent;
class QTimer;

// GutterController paints per-line git markers (added / modified / deleted)
// in a KTextEditor::Document's gutter. It owns one document and refreshes the
// core's git.file RPC for line hunks, driven by the core's git.invalidated
// notification (debounced to coalesce save bursts) plus a slow safety-net
// timer. Refreshing is gated on visibility: a document with no visible view
// stays quiet so background tabs cost nothing.
//
// One controller per open document — the parent owns it and destroys it when
// the document closes.
class GutterController : public QObject
{
    Q_OBJECT
public:
    GutterController(KTextEditor::Document *doc, const QString &absolutePath,
                     CoreClient *core, QObject *parent = nullptr);
    ~GutterController() override;

    QString filePath() const { return m_path; }

Q_SIGNALS:
    // Emitted whenever a fresh git.file reply has been applied. Lets the
    // status-bar widget reflect the active editor's branch / hunk counts
    // without doing its own RPC.
    void statusUpdated(const QString &path, const QString &branch,
                       const QString &status, int hunkCount);

protected:
    // Watches the document's views for Show / Hide so polling can be gated on
    // whether the file is actually on screen.
    bool eventFilter(QObject *watched, QEvent *event) override;

private:
    void registerMarkPixmaps();
    void pollNow();
    void applyHunks(const QJsonArray &hunks);
    void clearMarks();
    void scheduleAfterEdit();
    void scheduleRefresh();

    // True when at least one of the document's views is visible on screen.
    bool hasVisibleView() const;
    // Re-evaluate visibility and start/stop the safety-net timer to match;
    // a background document goes fully quiet.
    void updateVisibility();
    // Install the view watcher (Show / Hide) on a freshly created view.
    void watchView(KTextEditor::View *view);

    QPointer<KTextEditor::Document> m_doc;
    CoreClient *m_core = nullptr;
    QString m_path;
    QTimer *m_pollTimer = nullptr;
    QTimer *m_debounceTimer = nullptr;
    bool m_inFlight = false;
    bool m_visible = false;
    QString m_lastBranch;
    QString m_lastStatus;
    // The hunk array last painted into the gutter; an identical reply skips
    // the clear + re-add churn (and the gutter repaint it triggers).
    QJsonArray m_lastHunks;
};
