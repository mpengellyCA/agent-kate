// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

#include "GutterController.h"
#include "ipc/CoreClient.h"

#include <KTextEditor/Document>

#include <QIcon>
#include <QJsonObject>
#include <QJsonValue>
#include <QPainter>
#include <QPalette>
#include <QPixmap>
#include <QTimer>

namespace {
// Polling cadence and post-edit debounce. The cache on the core side handles
// concurrent reads, so 1 s is comfortable.
constexpr int kPollIntervalMs = 1000;
constexpr int kDebounceAfterEditMs = 2000;

// Custom mark slots, picked from the unreserved range (01..07 are taken by
// bookmarks / breakpoints / warning / error; 32 is SearchMatch).
constexpr uint kMarkAdded = 0x80;     // markType08
constexpr uint kMarkModified = 0x100; // markType09
constexpr uint kMarkDeleted = 0x200;  // markType10

// All marks owned by this controller, OR'd, for clearMarks() short-cut.
constexpr uint kOurMarks = kMarkAdded | kMarkModified | kMarkDeleted;

// Build a thin vertical stripe pixmap in `color`, sized for a gutter mark.
QPixmap stripePixmap(const QColor &color)
{
    QPixmap pm(4, 16);
    pm.fill(Qt::transparent);
    QPainter p(&pm);
    p.setRenderHint(QPainter::Antialiasing, false);
    p.fillRect(QRect(1, 0, 2, 16), color);
    return pm;
}

// Build a small right-pointing triangle for the deleted-hunk marker — there
// is no working-tree line to colour, so a thin triangle on the adjacent line
// flags the deletion.
QPixmap trianglePixmap(const QColor &color)
{
    QPixmap pm(8, 16);
    pm.fill(Qt::transparent);
    QPainter p(&pm);
    p.setRenderHint(QPainter::Antialiasing, true);
    QPolygon tri;
    tri << QPoint(1, 6) << QPoint(6, 8) << QPoint(1, 10);
    p.setBrush(color);
    p.setPen(Qt::NoPen);
    p.drawPolygon(tri);
    return pm;
}

QColor addedColor(const QPalette &pal)
{
    return pal.color(QPalette::Base).lightness() < 128
        ? QColor(0x5f, 0xd3, 0x8a) : QColor(0x1a, 0x7f, 0x37);
}

QColor modifiedColor(const QPalette &pal)
{
    return pal.color(QPalette::Base).lightness() < 128
        ? QColor(0x7c, 0xb7, 0xff) : QColor(0x1a, 0x5f, 0xb4);
}

QColor deletedColor(const QPalette &pal)
{
    return pal.color(QPalette::Base).lightness() < 128
        ? QColor(0xff, 0x8a, 0x80) : QColor(0xc0, 0x1c, 0x28);
}
} // namespace

GutterController::GutterController(KTextEditor::Document *doc, const QString &absolutePath,
                                   CoreClient *core, QObject *parent)
    : QObject(parent)
    , m_doc(doc)
    , m_core(core)
    , m_path(absolutePath)
    , m_pollTimer(new QTimer(this))
    , m_debounceTimer(new QTimer(this))
{
    m_pollTimer->setInterval(kPollIntervalMs);
    m_debounceTimer->setSingleShot(true);
    m_debounceTimer->setInterval(kDebounceAfterEditMs);

    registerMarkPixmaps();

    connect(m_pollTimer, &QTimer::timeout, this, &GutterController::pollNow);
    connect(m_debounceTimer, &QTimer::timeout, this, &GutterController::pollNow);

    // Skipping ticks while the user is mid-edit: each text change defers the
    // next poll by kDebounceAfterEditMs so the gutter doesn't churn against
    // half-typed code.
    connect(doc, &KTextEditor::Document::textChanged, this,
            [this] { scheduleAfterEdit(); });
    connect(doc, &KTextEditor::Document::aboutToClose, this,
            [this] {
                m_pollTimer->stop();
                m_debounceTimer->stop();
            });

    m_pollTimer->start();
    // One immediate poll so freshly opened files don't wait a second.
    QTimer::singleShot(0, this, &GutterController::pollNow);
}

GutterController::~GutterController()
{
    clearMarks();
}

void GutterController::registerMarkPixmaps()
{
    if (!m_doc) {
        return;
    }
    const QPalette pal;
    m_doc->setMarkIcon(static_cast<KTextEditor::Document::MarkTypes>(kMarkAdded),
                       QIcon(stripePixmap(addedColor(pal))));
    m_doc->setMarkIcon(static_cast<KTextEditor::Document::MarkTypes>(kMarkModified),
                       QIcon(stripePixmap(modifiedColor(pal))));
    m_doc->setMarkIcon(static_cast<KTextEditor::Document::MarkTypes>(kMarkDeleted),
                       QIcon(trianglePixmap(deletedColor(pal))));
    m_doc->setMarkDescription(static_cast<KTextEditor::Document::MarkTypes>(kMarkAdded),
                              QStringLiteral("Added (vs HEAD)"));
    m_doc->setMarkDescription(static_cast<KTextEditor::Document::MarkTypes>(kMarkModified),
                              QStringLiteral("Modified (vs HEAD)"));
    m_doc->setMarkDescription(static_cast<KTextEditor::Document::MarkTypes>(kMarkDeleted),
                              QStringLiteral("Deletion (vs HEAD)"));
}

void GutterController::scheduleAfterEdit()
{
    // Restart both timers: stop the regular ticker so it can't fire mid-edit,
    // and (re)arm the debounce so the next poll happens once the user pauses.
    m_pollTimer->stop();
    m_debounceTimer->start();
}

void GutterController::pollNow()
{
    if (m_inFlight || m_path.isEmpty() || !m_doc || !m_core->isConnected()) {
        // Always restart the regular ticker after the debounce fires so polls
        // resume their cadence.
        if (!m_pollTimer->isActive()) {
            m_pollTimer->start();
        }
        return;
    }
    m_inFlight = true;
    const QString path = m_path;
    m_core->call(QStringLiteral("git.file"),
                 QJsonObject{{QStringLiteral("path"), path}},
                 [this, path](const QJsonObject &result, const QJsonObject &error) {
                     m_inFlight = false;
                     if (!m_pollTimer->isActive()) {
                         m_pollTimer->start();
                     }
                     if (!m_doc || path != m_path) {
                         return; // file was closed or swapped while in flight
                     }
                     if (!error.isEmpty()) {
                         return;
                     }
                     applyHunks(result.value(QStringLiteral("hunks")).toArray());
                     m_lastBranch = result.value(QStringLiteral("branch")).toString();
                     m_lastStatus = result.value(QStringLiteral("status")).toString();
                     emit statusUpdated(m_path, m_lastBranch, m_lastStatus,
                                        result.value(QStringLiteral("hunks"))
                                            .toArray()
                                            .size());
                 });
}

void GutterController::clearMarks()
{
    if (!m_doc) {
        return;
    }
    const auto marks = m_doc->marks();
    for (auto it = marks.begin(); it != marks.end(); ++it) {
        const uint mine = it.value()->type & kOurMarks;
        if (mine != 0) {
            m_doc->removeMark(it.key(), mine);
        }
    }
}

void GutterController::applyHunks(const QJsonArray &hunks)
{
    if (!m_doc) {
        return;
    }
    clearMarks();
    const int lineCount = m_doc->lines();
    for (const QJsonValue &v : hunks) {
        const QJsonObject h = v.toObject();
        const QString kind = h.value(QStringLiteral("kind")).toString();
        const int newStart = h.value(QStringLiteral("newStart")).toInt();
        const int newLines = h.value(QStringLiteral("newLines")).toInt();
        if (kind == QLatin1String("delete")) {
            // No new-side line for pure deletions — mark the line just above
            // where the deletion happened so the user has something to click.
            const int marker = qBound(0, newStart - 1, lineCount - 1);
            if (marker >= 0 && marker < lineCount) {
                m_doc->addMark(marker, kMarkDeleted);
            }
            continue;
        }
        const uint markType = (kind == QLatin1String("add")) ? kMarkAdded : kMarkModified;
        // Hunks come back 1-based; KTextEditor's lines are 0-based.
        for (int i = 0; i < newLines; ++i) {
            const int line = newStart - 1 + i;
            if (line >= 0 && line < lineCount) {
                m_doc->addMark(line, markType);
            }
        }
    }
}
