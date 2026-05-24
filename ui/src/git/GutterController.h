// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#pragma once

#include <QJsonArray>
#include <QObject>
#include <QPointer>
#include <QString>

namespace KTextEditor {
class Document;
}
class CoreClient;
class QTimer;

// GutterController paints per-line git markers (added / modified / deleted)
// in a KTextEditor::Document's gutter. It owns one document and polls the
// core's git.file RPC for line hunks, debouncing 2 s after the most recent
// keystroke so editing the file does not flood the bus.
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

private:
    void registerMarkPixmaps();
    void pollNow();
    void applyHunks(const QJsonArray &hunks);
    void clearMarks();
    void scheduleAfterEdit();

    QPointer<KTextEditor::Document> m_doc;
    CoreClient *m_core = nullptr;
    QString m_path;
    QTimer *m_pollTimer = nullptr;
    QTimer *m_debounceTimer = nullptr;
    bool m_inFlight = false;
    QString m_lastBranch;
    QString m_lastStatus;
};
