// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <KTextEditor/AnnotationInterface>

#include <QDateTime>
#include <QObject>
#include <QPointer>
#include <QString>
#include <QVector>

namespace KTextEditor {
class Document;
class View;
}
class CoreClient;

// BlameLine mirrors gitstatus.BlameLine on the wire — one row per source
// line, populated from the git.blame RPC.
struct UiBlameLine {
    QString sha;
    QString author;
    QString summary;
    QDateTime authored;
};

// BlameAnnotationModel feeds KTextEditor's annotation border. Lives as long
// as its owning BlameController. Display text is "sha author"; tooltip is
// the full commit subject + date.
class BlameAnnotationModel : public KTextEditor::AnnotationModel
{
    Q_OBJECT
public:
    void setLines(QVector<UiBlameLine> lines);
    QVariant data(int line, Qt::ItemDataRole role) const override;

private:
    QVector<UiBlameLine> m_lines;
};

// BlameController attaches / detaches a BlameAnnotationModel to one
// KTextEditor::View, fetching git.blame on demand. The MainWindow keeps one
// per Document and toggles them through a View-menu action.
class BlameController : public QObject
{
    Q_OBJECT
public:
    BlameController(KTextEditor::Document *doc, KTextEditor::View *view,
                    const QString &absolutePath, CoreClient *core,
                    QObject *parent = nullptr);
    ~BlameController() override;

    bool isEnabled() const { return m_enabled; }
    void setEnabled(bool on);
    void refresh();

private:
    void applyData(const QVector<UiBlameLine> &lines);

    QPointer<KTextEditor::Document> m_doc;
    QPointer<KTextEditor::View> m_view;
    CoreClient *m_core = nullptr;
    QString m_path;
    BlameAnnotationModel *m_model = nullptr;
    bool m_enabled = false;
    bool m_inFlight = false;
};
