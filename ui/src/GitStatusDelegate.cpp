// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "GitStatusDelegate.h"

#include <QApplication>
#include <QDir>
#include <QFileSystemModel>
#include <QIcon>
#include <QPainter>
#include <QStyle>
#include <QStyleOptionViewItem>
#include <QWidget>

namespace {
// Map a status code to a Breeze VCS emblem icon name. Untracked deliberately
// has no emblem (it is conveyed only by the dimmed text tint) to avoid a sea
// of badges on a fresh checkout.
QString emblemForStatus(int status)
{
    switch (status) {
    case GitStatusDelegate::Modified:
        return QStringLiteral("vcs-locally-modified");
    case GitStatusDelegate::Added:
        return QStringLiteral("vcs-added");
    case GitStatusDelegate::Deleted:
        return QStringLiteral("vcs-removed");
    case GitStatusDelegate::Renamed:
        return QStringLiteral("vcs-locally-modified");
    case GitStatusDelegate::Conflicted:
        return QStringLiteral("vcs-conflicting");
    default:
        return QString();
    }
}

// Pick the QPalette role used to tint the entry's text. Conflicts use the
// link-visited / highlight-ish accent via BrightText fallback is avoided —
// instead we lean on roles that exist in every Breeze scheme. Untracked and
// clean stay on the normal text role (untracked is dimmed via PlaceholderText).
QPalette::ColorRole roleForStatus(int status)
{
    switch (status) {
    case GitStatusDelegate::Untracked:
        return QPalette::PlaceholderText;
    case GitStatusDelegate::Conflicted:
        return QPalette::LinkVisited;
    case GitStatusDelegate::Added:
    case GitStatusDelegate::Modified:
    case GitStatusDelegate::Renamed:
        return QPalette::Link;
    default:
        return QPalette::Text;
    }
}
} // namespace

GitStatusDelegate::GitStatusDelegate(QFileSystemModel *fsModel, QObject *parent)
    : QStyledItemDelegate(parent)
    , m_fsModel(fsModel)
{
}

bool GitStatusDelegate::setStatuses(QHash<QString, int> statuses)
{
    if (statuses == m_statuses) {
        return false;
    }
    m_statuses = std::move(statuses);
    return true;
}

int GitStatusDelegate::statusForIndex(const QModelIndex &index) const
{
    if (m_statuses.isEmpty() || !index.isValid()) {
        return Clean;
    }
    // FilePathRole travels through any proxy that forwards data(), so this works
    // whether the tree views the QFileSystemModel directly or via a filter
    // proxy. Fall back to nothing if the role is unavailable.
    const QString path =
        index.data(QFileSystemModel::FilePathRole).toString();
    if (path.isEmpty()) {
        return Clean;
    }
    return m_statuses.value(QDir::cleanPath(path), Clean);
}

void GitStatusDelegate::paint(QPainter *painter, const QStyleOptionViewItem &option,
                              const QModelIndex &index) const
{
    const int status = statusForIndex(index);
    if (status == Clean) {
        QStyledItemDelegate::paint(painter, option, index);
        return;
    }

    // Tint the text without overriding the Breeze style: mutate a copy of the
    // option's palette text/highlighted-text colours from a QPalette role, then
    // let the base delegate render normally so selection, focus, and the file
    // icon all stay native.
    QStyleOptionViewItem opt(option);
    const QPalette::ColorRole role = roleForStatus(status);
    if (role != QPalette::Text) {
        const QColor tint = opt.palette.color(QPalette::Active, role);
        opt.palette.setColor(QPalette::Text, tint);
        opt.palette.setColor(QPalette::Inactive, QPalette::Text, tint);
        // Leave HighlightedText alone so selected rows stay legible against the
        // highlight; the emblem still conveys the status there.
    }

    QStyledItemDelegate::paint(painter, opt, index);

    const QString emblemName = emblemForStatus(status);
    if (emblemName.isEmpty()) {
        return;
    }
    const QIcon emblem = QIcon::fromTheme(emblemName);
    if (emblem.isNull()) {
        return;
    }

    // Locate the actual icon rect from the style so the emblem sits over the
    // file icon — not the row's left edge — regardless of tree indentation.
    QStyleOptionViewItem iconOpt(opt);
    initStyleOption(&iconOpt, index);
    const QWidget *w = iconOpt.widget;
    const QStyle *style = w ? w->style() : QApplication::style();
    QRect deco = style->subElementRect(QStyle::SE_ItemViewItemDecoration, &iconOpt, w);
    if (deco.isEmpty()) {
        deco = QRect(option.rect.left(), option.rect.top(),
                     option.decorationSize.width(), option.rect.height());
    }

    // Draw a small emblem in the bottom-right corner of the icon.
    const int sz = qMax(8, deco.height() / 2);
    const int x = deco.right() - sz + 1;
    const int y = deco.bottom() - sz + 1;
    painter->save();
    painter->setRenderHint(QPainter::SmoothPixmapTransform, true);
    emblem.paint(painter, QRect(x, y, sz, sz));
    painter->restore();
}
