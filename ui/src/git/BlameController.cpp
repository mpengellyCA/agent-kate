// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "BlameController.h"
#include "ipc/CoreClient.h"

#include <KTextEditor/Document>
#include <KTextEditor/View>

#include <QColor>
#include <QDateTime>
#include <QJsonArray>
#include <QJsonObject>
#include <QJsonValue>
#include <QPalette>

void BlameAnnotationModel::setLines(QVector<UiBlameLine> lines)
{
    m_lines = std::move(lines);
    emit reset();
}

QVariant BlameAnnotationModel::data(int line, Qt::ItemDataRole role) const
{
    if (line < 0 || line >= m_lines.size()) {
        return {};
    }
    const UiBlameLine &b = m_lines.at(line);
    if (b.sha.isEmpty()) {
        return {};
    }
    // Collapse repeated headers: the same commit on consecutive lines only
    // shows its label on the first line, leaving the rest blank for a tidy
    // grouped column. Tooltip still resolves per line.
    const bool first = (line == 0) || (m_lines.at(line - 1).sha != b.sha);
    switch (role) {
    case Qt::DisplayRole:
        if (!first) {
            return QString();
        }
        return QStringLiteral("%1  %2").arg(b.sha, b.author);
    case Qt::ToolTipRole:
        return QStringLiteral("%1  •  %2\n%3")
            .arg(b.sha, b.author, b.summary)
            + (b.authored.isValid()
                   ? QStringLiteral("\n%1").arg(b.authored.toString(Qt::ISODate))
                   : QString());
    case Qt::ForegroundRole: {
        QPalette pal;
        QColor c = pal.color(QPalette::WindowText);
        c.setAlpha(160);
        return c;
    }
    case Qt::BackgroundRole: {
        // Alternate background per consecutive commit run so the eye can
        // see commit boundaries at a glance.
        QPalette pal;
        const bool dark = pal.color(QPalette::Base).lightness() < 128;
        int run = 0;
        for (int i = line; i > 0; --i) {
            if (m_lines.at(i).sha != m_lines.at(i - 1).sha) {
                break;
            }
            ++run;
        }
        // run is the distance back to the most recent boundary; use a
        // checksum of the sha to pick a stable shade per commit instead of
        // alternating mechanically (which jitters around 1-line commits).
        uint h = qHash(b.sha);
        QColor base = pal.color(QPalette::AlternateBase);
        if ((h & 1) == 0) {
            base = base.lighter(dark ? 115 : 96);
        }
        Q_UNUSED(run);
        return base;
    }
    default:
        return {};
    }
}

BlameController::BlameController(KTextEditor::Document *doc, KTextEditor::View *view,
                                 const QString &absolutePath, CoreClient *core,
                                 QObject *parent)
    : QObject(parent)
    , m_doc(doc)
    , m_view(view)
    , m_core(core)
    , m_path(absolutePath)
    , m_model(new BlameAnnotationModel)
{
    m_model->setParent(this);
    // The blame border updates lazily as the user types — fall back to a
    // simple "refetch on document save" trigger; live re-blame on every
    // keystroke would be expensive and noisy.
    connect(doc, &KTextEditor::Document::documentSavedOrUploaded, this,
            [this](KTextEditor::Document *, bool) {
                if (m_enabled) {
                    refresh();
                }
            });
    // An agent rewriting this open file triggers a silent documentReload — not a
    // user save and not a HEAD move — so refresh blame on reload too, otherwise
    // the annotations track the pre-edit contents.
    connect(doc, &KTextEditor::Document::reloaded, this,
            [this](KTextEditor::Document *) {
                if (m_enabled) {
                    refresh();
                }
            });
    // Blame vs HEAD only changes when HEAD moves (a commit lands / branch
    // switches) or this file is saved — the save is handled by the
    // documentSavedOrUploaded hook above. A plain git.invalidated fires on
    // every unrelated working-tree change, so refetching on it re-shelled
    // `git blame` for nothing. Gate on git.log.invalidated, which the core
    // emits only when a thread's HEAD actually moves.
    connect(m_core, &CoreClient::notification, this,
            [this](const QString &m, const QJsonObject &) {
                if (m_enabled && m == QLatin1String("git.log.invalidated")) {
                    refresh();
                }
            });
}

BlameController::~BlameController()
{
    if (m_view && m_enabled) {
        m_view->setAnnotationBorderVisible(false);
        m_view->setAnnotationModel(nullptr);
    }
}

void BlameController::setEnabled(bool on)
{
    if (on == m_enabled || !m_view) {
        return;
    }
    m_enabled = on;
    if (on) {
        m_view->setAnnotationModel(m_model);
        m_view->setAnnotationBorderVisible(true);
        refresh();
    } else {
        m_view->setAnnotationBorderVisible(false);
        m_view->setAnnotationModel(nullptr);
    }
}

void BlameController::refresh()
{
    if (m_inFlight || m_path.isEmpty() || !m_core->isConnected()) {
        return;
    }
    m_inFlight = true;
    const QString path = m_path;
    m_core->call(QStringLiteral("git.blame"),
                 QJsonObject{{QStringLiteral("path"), path}},
                 [this, path](const QJsonObject &result, const QJsonObject &error) {
                     m_inFlight = false;
                     if (!m_view || path != m_path || !error.isEmpty()) {
                         return;
                     }
                     const QJsonArray arr =
                         result.value(QStringLiteral("lines")).toArray();
                     QVector<UiBlameLine> lines;
                     lines.reserve(arr.size());
                     for (const QJsonValue &v : arr) {
                         const QJsonObject o = v.toObject();
                         UiBlameLine b;
                         b.sha = o.value(QStringLiteral("sha")).toString();
                         b.author = o.value(QStringLiteral("author")).toString();
                         b.summary = o.value(QStringLiteral("summary")).toString();
                         b.authored = QDateTime::fromString(
                             o.value(QStringLiteral("authorTime")).toString(),
                             Qt::ISODate);
                         lines.append(b);
                     }
                     applyData(lines);
                 },
                 this);
}

void BlameController::applyData(const QVector<UiBlameLine> &lines)
{
    m_model->setLines(lines);
}
