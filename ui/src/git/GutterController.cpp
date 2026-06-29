// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "GutterController.h"
#include "ipc/CoreClient.h"
#include "theme/ThemeManager.h"

#include <KLocalizedString>
#include <KTextEditor/Document>
#include <KTextEditor/View>

#include <QEvent>
#include <QIcon>
#include <QJsonObject>
#include <QJsonValue>
#include <QPainter>
#include <QPalette>
#include <QPixmap>
#include <QTimer>

namespace {
// The gutter is driven by the core's git.invalidated notification; this slow
// timer is only a safety net in case a working-tree change slips past the fs
// watcher. The post-edit debounce keeps a mid-edit save from painting against
// half-typed code, and the invalidated debounce coalesces save bursts.
constexpr int kPollIntervalMs = 20000;
constexpr int kDebounceAfterEditMs = 2000;
constexpr int kInvalidatedDebounceMs = 250;

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
    Q_UNUSED(pal);
    return ThemeManager::palette().positive;
}

QColor modifiedColor(const QPalette &pal)
{
    Q_UNUSED(pal);
    return ThemeManager::palette().info;
}

QColor deletedColor(const QPalette &pal)
{
    Q_UNUSED(pal);
    return ThemeManager::palette().negative;
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
    m_debounceTimer->setInterval(kInvalidatedDebounceMs);

    registerMarkPixmaps();

    // The mark icons are baked into pixmaps from the active theme's semantic
    // colours, so re-bake them when the theme switches.
    connect(ThemeManager::instance(), &ThemeManager::changed, this,
            &GutterController::registerMarkPixmaps);

    connect(m_pollTimer, &QTimer::timeout, this, &GutterController::pollNow);
    connect(m_debounceTimer, &QTimer::timeout, this, &GutterController::pollNow);

    // The gutter is normally refreshed by git.invalidated (debounced), but a
    // mid-edit save should not paint against half-typed code: each text change
    // defers the next refresh by kDebounceAfterEditMs so the user can pause.
    connect(doc, &KTextEditor::Document::textChanged, this,
            [this] { scheduleAfterEdit(); });
    connect(doc, &KTextEditor::Document::aboutToClose, this,
            [this] {
                m_pollTimer->stop();
                m_debounceTimer->stop();
            });
    // An external rewrite (e.g. an agent edits this open file) triggers a silent
    // documentReload, which clears KTextEditor's marks. Drop the dedup baseline
    // so applyHunks cannot suppress the repaint, and re-poll to restore stripes.
    connect(doc, &KTextEditor::Document::reloaded, this,
            [this](KTextEditor::Document *) {
                m_lastHunks = QJsonArray();
                scheduleRefresh();
            });

    // The fs watcher pushes git.invalidated on every working-tree change. That
    // is the primary refresh trigger — coalesce bursts (e.g. a save touching
    // several files) into one refresh, and only while the file is on screen.
    if (m_core) {
        connect(m_core, &CoreClient::notification, this,
                [this](const QString &method, const QJsonObject &) {
                    if (method == QLatin1String("git.invalidated")) {
                        scheduleRefresh();
                    }
                });
    }

    // Watch the document's views for Show / Hide so a backgrounded tab stops
    // polling. New views (split, reopen) get the same watcher.
    const auto views = doc->views();
    for (KTextEditor::View *v : views) {
        watchView(v);
    }
    connect(doc, &KTextEditor::Document::viewCreated, this,
            [this](KTextEditor::Document *, KTextEditor::View *view) {
                watchView(view);
                updateVisibility();
            });

    // Establish initial visibility (starts the safety-net timer if on screen)
    // and do one immediate refresh so freshly opened files don't wait.
    updateVisibility();
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
                              i18nc("git gutter mark", "Added (vs HEAD)"));
    m_doc->setMarkDescription(static_cast<KTextEditor::Document::MarkTypes>(kMarkModified),
                              i18nc("git gutter mark", "Modified (vs HEAD)"));
    m_doc->setMarkDescription(static_cast<KTextEditor::Document::MarkTypes>(kMarkDeleted),
                              i18nc("git gutter mark", "Deletion (vs HEAD)"));
}

void GutterController::scheduleAfterEdit()
{
    if (!m_visible) {
        return; // a background document does not chase its own edits
    }
    // Hold the refresh until the user pauses: (re)arm the debounce with the
    // longer post-edit interval so the gutter does not churn against
    // half-typed code.
    m_debounceTimer->start(kDebounceAfterEditMs);
}

void GutterController::scheduleRefresh()
{
    if (!m_visible) {
        return; // off-screen documents stay quiet until shown
    }
    // Coalesce bursts of git.invalidated (a save can touch several files) into
    // a single refresh. Don't stretch an already-armed post-edit hold.
    if (!m_debounceTimer->isActive()) {
        m_debounceTimer->start(kInvalidatedDebounceMs);
    }
}

bool GutterController::hasVisibleView() const
{
    if (!m_doc) {
        return false;
    }
    const auto views = m_doc->views();
    for (KTextEditor::View *v : views) {
        if (v && v->isVisible()) {
            return true;
        }
    }
    return false;
}

void GutterController::watchView(KTextEditor::View *view)
{
    if (view) {
        view->installEventFilter(this);
    }
}

void GutterController::updateVisibility()
{
    const bool nowVisible = hasVisibleView();
    if (nowVisible == m_visible) {
        return;
    }
    m_visible = nowVisible;
    if (m_visible) {
        // Came on screen: resume the safety net and refresh promptly so the
        // gutter reflects any change that happened while hidden.
        if (!m_pollTimer->isActive()) {
            m_pollTimer->start();
        }
        scheduleRefresh();
    } else {
        // Went off screen: go fully quiet.
        m_pollTimer->stop();
        m_debounceTimer->stop();
    }
}

bool GutterController::eventFilter(QObject *watched, QEvent *event)
{
    if (event->type() == QEvent::Show || event->type() == QEvent::Hide) {
        updateVisibility();
    }
    return QObject::eventFilter(watched, event);
}

void GutterController::pollNow()
{
    // Off-screen documents never poll: keep the timers stopped and bail. The
    // immediate refresh on construction can also land before the view is shown
    // — visibility decides, not the source of the call.
    if (!m_visible) {
        return;
    }
    if (m_inFlight || m_path.isEmpty() || !m_doc || !m_core->isConnected()) {
        // Restart the safety-net ticker so refreshes keep their cadence while
        // the file stays visible.
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
                     if (m_visible && !m_pollTimer->isActive()) {
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
                 },
                 this);
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
    // git.invalidated fires for any working-tree change in the worktree, so a
    // refresh often returns hunks identical to what is already painted. Skip
    // the clear + re-add (and the gutter repaint it triggers) in that case.
    if (hunks == m_lastHunks) {
        return;
    }
    m_lastHunks = hunks;
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
