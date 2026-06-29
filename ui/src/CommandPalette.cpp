// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "CommandPalette.h"

#include <KLocalizedString>

#include <QAction>
#include <QApplication>
#include <QColor>
#include <QEvent>
#include <QFontMetrics>
#include <QKeyEvent>
#include <QKeySequence>
#include <QLineEdit>
#include <QListWidget>
#include <QListWidgetItem>
#include <QMetaObject>
#include <QModelIndex>
#include <QPainter>
#include <QPalette>
#include <QPoint>
#include <QRect>
#include <QScreen>
#include <QSet>
#include <QShowEvent>
#include <QSize>
#include <QStyle>
#include <QStyleOptionViewItem>
#include <QStyledItemDelegate>
#include <QVBoxLayout>
#include <QWidget>

#include <algorithm>

namespace {

// Roles we stash on each list item so the delegate can paint command text and a
// right-aligned, muted shortcut, and so triggerCurrent() can recover the source
// command index.
constexpr int kShortcutRole = Qt::UserRole + 1;
constexpr int kCommandIndexRole = Qt::UserRole + 2;

// Strip Qt mnemonic ampersands from an action's text. "&File" -> "File",
// "Save && Close" -> "Save & Close". We collapse a doubled "&&" to a single
// literal ampersand and drop any remaining single "&".
QString stripMnemonics(const QString &text)
{
    QString out;
    out.reserve(text.size());
    for (int i = 0; i < text.size(); ++i) {
        const QChar c = text.at(i);
        if (c == QLatin1Char('&')) {
            if (i + 1 < text.size() && text.at(i + 1) == QLatin1Char('&')) {
                out.append(QLatin1Char('&'));
                ++i; // consume the second '&'
            }
            // else: a single '&' is a mnemonic marker — drop it.
            continue;
        }
        out.append(c);
    }
    return out.trimmed();
}

// Fuzzy match result. matched is false when the query is not a subsequence of
// the candidate at all; otherwise score ranks the quality of the match (higher
// is better) so callers can sort the survivors.
struct MatchResult {
    bool matched = false;
    int score = 0;
};

// Score how well `query` matches `text` (both compared case-insensitively via
// the pre-lowered haystack). The query must appear as an in-order subsequence
// of the text to match at all. Ranking, best to worst:
//   * exact prefix              (query is a prefix of text)
//   * contiguous substring      (query appears verbatim somewhere)
//   * word-boundary subsequence (each query char begins a word in text)
//   * loose subsequence         (any in-order char run)
// Shorter candidates and earlier matches are nudged higher so "New" beats
// "New Terminal From Here" for the query "new".
MatchResult fuzzyMatch(const QString &queryLower, const QString &textLower)
{
    MatchResult r;
    if (queryLower.isEmpty()) {
        r.matched = true;
        r.score = 1; // everything matches an empty query, all equal-ish
        return r;
    }
    if (textLower.isEmpty()) {
        return r;
    }

    // First, must be a subsequence at all.
    int qi = 0;
    bool wordBoundaryOnly = true;
    bool prevWasSep = true; // start-of-string counts as a boundary
    for (int ti = 0; ti < textLower.size() && qi < queryLower.size(); ++ti) {
        const QChar tc = textLower.at(ti);
        const bool isSep = !tc.isLetterOrNumber();
        if (tc == queryLower.at(qi)) {
            if (!prevWasSep) {
                // Matched a character that is mid-word; not a pure
                // word-boundary subsequence.
                wordBoundaryOnly = false;
            }
            ++qi;
        }
        prevWasSep = isSep;
    }
    if (qi < queryLower.size()) {
        return r; // not even a subsequence
    }
    r.matched = true;

    // Base score, then layer bonuses.
    int score = 100;

    if (textLower.startsWith(queryLower)) {
        score += 1000;
    } else {
        const int idx = textLower.indexOf(queryLower);
        if (idx >= 0) {
            // Contiguous substring; earlier is better.
            score += 500 - qMin(idx, 200);
            // A substring sitting on a word boundary is nicer still.
            if (idx == 0 || !textLower.at(idx - 1).isLetterOrNumber()) {
                score += 150;
            }
        } else if (wordBoundaryOnly) {
            score += 300;
        }
        // else: loose subsequence keeps the base score only.
    }

    // Prefer shorter candidates so the most specific command wins ties.
    score -= qMin(textLower.size(), 80);

    r.score = score;
    return r;
}

// Renders "command text .......... Shortcut": the command text on the left in
// the normal text colour (HighlightedText when the row is selected) and the
// shortcut right-aligned in a muted PlaceholderText colour. Icons are painted
// by the view from Qt::DecorationRole; the delegate only lays out text so it
// composes with the default decoration handling.
class CommandDelegate : public QStyledItemDelegate
{
public:
    explicit CommandDelegate(QObject *parent = nullptr)
        : QStyledItemDelegate(parent)
    {
    }

    QSize sizeHint(const QStyleOptionViewItem &option,
                   const QModelIndex &index) const override
    {
        QSize s = QStyledItemDelegate::sizeHint(option, index);
        // Comfortable, VS-Code-ish row height.
        s.setHeight(qMax(s.height(), option.fontMetrics.height() + 14));
        return s;
    }

    void paint(QPainter *painter, const QStyleOptionViewItem &option,
               const QModelIndex &index) const override
    {
        QStyleOptionViewItem opt(option);
        initStyleOption(&opt, index);

        const QWidget *w = opt.widget;
        QStyle *style = w ? w->style() : QApplication::style();

        const bool selected = opt.state & QStyle::State_Selected;

        // Let the style paint the background (selection highlight, hover) and
        // the decoration (icon) — but suppress its text so we own the layout.
        const QString text = opt.text;
        opt.text.clear();
        style->drawControl(QStyle::CE_ItemViewItem, &opt, painter, w);

        // Text rectangle (right of any icon, inset for breathing room).
        QRect textRect =
            style->subElementRect(QStyle::SE_ItemViewItemText, &opt, w);
        textRect.adjust(2, 0, -8, 0);
        if (!textRect.isValid()) {
            return;
        }

        const QPalette &pal = opt.palette;
        const QColor textColor =
            selected ? pal.color(QPalette::Active, QPalette::HighlightedText)
                     : pal.color(QPalette::Active, QPalette::Text);
        QColor shortcutColor =
            selected ? pal.color(QPalette::Active, QPalette::HighlightedText)
                     : pal.color(QPalette::Active, QPalette::PlaceholderText);
        if (!selected) {
            shortcutColor.setAlpha(200);
        }

        const QString shortcut = index.data(kShortcutRole).toString();
        const QFontMetrics fm(opt.font);

        painter->save();
        painter->setFont(opt.font);

        int rightReserved = 0;
        if (!shortcut.isEmpty()) {
            const int scWidth = fm.horizontalAdvance(shortcut);
            const QRect scRect(textRect.right() - scWidth, textRect.top(),
                               scWidth, textRect.height());
            painter->setPen(shortcutColor);
            painter->drawText(scRect, Qt::AlignVCenter | Qt::AlignRight,
                              shortcut);
            rightReserved = scWidth + 16; // gap before the command text ends
        }

        QRect cmdRect = textRect;
        cmdRect.setRight(textRect.right() - rightReserved);
        const QString elided =
            fm.elidedText(text, Qt::ElideRight, cmdRect.width());
        painter->setPen(textColor);
        painter->drawText(cmdRect, Qt::AlignVCenter | Qt::AlignLeft, elided);

        painter->restore();
    }
};

} // namespace

CommandPalette::CommandPalette(QWidget *parent)
    : QDialog(parent)
{
    // A frameless, translucent card: a rounded panel painted by the stylesheet
    // on top of a translucent dialog window so the corners read as rounded.
    setWindowFlags(Qt::Dialog | Qt::FramelessWindowHint);
    setAttribute(Qt::WA_TranslucentBackground);
    setModal(true);
    setSizeGripEnabled(false);
    resize(620, 420);

    auto *outer = new QVBoxLayout(this);
    // Margin around the card so a drop-shadow-ish border has room to breathe.
    outer->setContentsMargins(0, 0, 0, 0);

    // The visible rounded card. Everything lives inside it.
    auto *card = new QWidget(this);
    card->setObjectName(QStringLiteral("commandPaletteCard"));
    outer->addWidget(card);

    auto *layout = new QVBoxLayout(card);
    layout->setContentsMargins(12, 12, 12, 12);
    layout->setSpacing(8);

    m_search = new QLineEdit(card);
    m_search->setObjectName(QStringLiteral("commandPaletteSearch"));
    m_search->setClearButtonEnabled(true);
    m_search->setPlaceholderText(
        i18n("Type a command…  (e.g. 'theme', 'terminal', 'commit')"));
    layout->addWidget(m_search);

    m_list = new QListWidget(card);
    m_list->setObjectName(QStringLiteral("commandPaletteList"));
    m_list->setUniformItemSizes(false);
    m_list->setSelectionMode(QAbstractItemView::SingleSelection);
    m_list->setHorizontalScrollBarPolicy(Qt::ScrollBarAlwaysOff);
    m_list->setVerticalScrollBarPolicy(Qt::ScrollBarAsNeeded);
    m_list->setFocusPolicy(Qt::NoFocus); // keep keyboard focus in the search box
    m_list->setItemDelegate(new CommandDelegate(m_list));
    layout->addWidget(m_list, 1);

    // Style the card with the app palette: a translucent navy panel, accent
    // border, comfortable padding, and an accent-highlighted selected row.
    const QPalette pal = palette();
    const QColor base = pal.color(QPalette::Base);
    const QColor accent = pal.color(QPalette::Accent);
    const QColor highlight = pal.color(QPalette::Highlight);
    const QColor highlightedText = pal.color(QPalette::HighlightedText);
    const QColor window = pal.color(QPalette::Window);

    setStyleSheet(QStringLiteral(
                      "#commandPaletteCard {"
                      "  background-color: %1;"
                      "  border: 1px solid %2;"
                      "  border-radius: 10px;"
                      "}"
                      "#commandPaletteSearch {"
                      "  padding: 7px 10px;"
                      "  border: 1px solid %3;"
                      "  border-radius: 6px;"
                      "  background-color: %4;"
                      "}"
                      "#commandPaletteList {"
                      "  border: none;"
                      "  background: transparent;"
                      "  outline: none;"
                      "}"
                      "#commandPaletteList::item {"
                      "  padding: 4px 6px;"
                      "  border-radius: 5px;"
                      "}"
                      "#commandPaletteList::item:selected {"
                      "  background-color: %5;"
                      "  color: %6;"
                      "}")
                      .arg(window.name(QColor::HexArgb),
                           accent.name(QColor::HexArgb),
                           accent.name(QColor::HexArgb),
                           base.name(QColor::HexArgb),
                           highlight.name(QColor::HexArgb),
                           highlightedText.name(QColor::HexArgb)));

    connect(m_search, &QLineEdit::textChanged, this,
            [this](const QString &q) { rebuildList(q); });

    // Click (or activate) a row triggers it, like Enter on the selection.
    connect(m_list, &QListWidget::itemClicked, this,
            [this](QListWidgetItem *) { triggerCurrent(); });

    // The search box keeps focus, so route navigation/commit/dismiss keys from
    // it into the list via an event filter.
    m_search->installEventFilter(this);
}

void CommandPalette::setActions(const QList<QAction *> &actions)
{
    m_commands.clear();
    m_commands.reserve(actions.size());

    // De-duplicate by (display text, shortcut) so the same command surfaced via
    // both a menu and a toolbar appears once.
    QSet<QString> seen;
    for (QAction *action : actions) {
        if (!action || action->isSeparator() || !action->isVisible()
            || !action->isEnabled()) {
            continue;
        }
        const QString display = stripMnemonics(action->text());
        if (display.isEmpty()) {
            continue;
        }
        const QString shortcut =
            action->shortcut().toString(QKeySequence::NativeText);

        const QString key = display + QLatin1Char('\x1f') + shortcut;
        if (seen.contains(key)) {
            continue;
        }
        seen.insert(key);

        Command cmd;
        cmd.text = display;
        cmd.shortcut = shortcut;
        cmd.lowerText = display.toLower();
        cmd.action = action;
        m_commands.append(cmd);
    }

    // Stable alphabetical base order so an empty query shows a tidy list.
    std::sort(m_commands.begin(), m_commands.end(),
              [](const Command &a, const Command &b) {
                  return a.text.localeAwareCompare(b.text) < 0;
              });

    if (isVisible()) {
        rebuildList(m_search ? m_search->text() : QString());
    }
}

void CommandPalette::showPalette()
{
    if (m_search) {
        m_search->clear();
    }
    rebuildList(QString());
    show();
    raise();
    activateWindow();
    if (m_search) {
        m_search->setFocus(Qt::OtherFocusReason);
    }
}

void CommandPalette::showEvent(QShowEvent *event)
{
    QDialog::showEvent(event);

    // Centre horizontally on the parent (or the screen) and anchor ~80px from
    // its top, VS-Code style.
    QRect anchor;
    if (QWidget *p = parentWidget()) {
        anchor = QRect(p->mapToGlobal(QPoint(0, 0)), p->size());
    } else if (QScreen *screen = this->screen()) {
        anchor = screen->availableGeometry();
    }

    if (anchor.isValid()) {
        const int x = anchor.left() + (anchor.width() - width()) / 2;
        const int y = anchor.top() + 80;
        move(qMax(anchor.left(), x), qMax(anchor.top(), y));
    }
}

void CommandPalette::rebuildList(const QString &query)
{
    if (!m_list) {
        return;
    }
    m_list->clear();

    const QString q = query.trimmed().toLower();

    // Score and collect matches, remembering each survivor's source index.
    struct Scored {
        int score;
        int commandIndex;
    };
    QVector<Scored> scored;
    scored.reserve(m_commands.size());
    for (int i = 0; i < m_commands.size(); ++i) {
        const MatchResult m = fuzzyMatch(q, m_commands.at(i).lowerText);
        if (m.matched) {
            scored.append(Scored{m.score, i});
        }
    }

    // Higher score first; ties keep the alphabetical base order (stable sort on
    // ascending command index for equal scores).
    std::stable_sort(scored.begin(), scored.end(),
                     [](const Scored &a, const Scored &b) {
                         if (a.score != b.score) {
                             return a.score > b.score;
                         }
                         return a.commandIndex < b.commandIndex;
                     });

    for (const Scored &s : scored) {
        const Command &cmd = m_commands.at(s.commandIndex);
        auto *item = new QListWidgetItem(cmd.text, m_list);
        item->setData(kShortcutRole, cmd.shortcut);
        item->setData(kCommandIndexRole, s.commandIndex);
        if (cmd.action && !cmd.action->icon().isNull()) {
            item->setIcon(cmd.action->icon());
        }
        // Tooltip surfaces the full text even when the row elides.
        QString tip = cmd.text;
        if (!cmd.shortcut.isEmpty()) {
            tip += QStringLiteral("  (%1)").arg(cmd.shortcut);
        }
        item->setToolTip(tip);
    }

    // Default selection on the first (best-ranked) row so Enter is immediately
    // useful.
    if (m_list->count() > 0) {
        m_list->setCurrentRow(0);
    }
}

void CommandPalette::moveSelection(int delta)
{
    if (!m_list || m_list->count() == 0) {
        return;
    }
    int row = m_list->currentRow();
    if (row < 0) {
        row = 0;
    } else {
        row += delta;
    }
    // Wrap around so Up from the top lands on the bottom and vice versa.
    const int n = m_list->count();
    row = ((row % n) + n) % n;
    m_list->setCurrentRow(row);
    m_list->scrollToItem(m_list->currentItem(),
                         QAbstractItemView::EnsureVisible);
}

QAction *CommandPalette::actionForRow(int row) const
{
    if (!m_list || row < 0 || row >= m_list->count()) {
        return nullptr;
    }
    const QListWidgetItem *item = m_list->item(row);
    if (!item) {
        return nullptr;
    }
    const int idx = item->data(kCommandIndexRole).toInt();
    if (idx < 0 || idx >= m_commands.size()) {
        return nullptr;
    }
    return m_commands.at(idx).action.data();
}

void CommandPalette::triggerCurrent()
{
    QAction *action = actionForRow(m_list ? m_list->currentRow() : -1);

    // Close the palette *before* triggering so the action runs against the
    // underlying window, not the popup. Defer the trigger onto the event loop
    // so the dialog has fully closed and focus has returned first.
    accept();

    if (action) {
        QMetaObject::invokeMethod(action, "trigger", Qt::QueuedConnection);
    }
}

bool CommandPalette::eventFilter(QObject *watched, QEvent *event)
{
    if (watched == m_search && event->type() == QEvent::KeyPress) {
        auto *ke = static_cast<QKeyEvent *>(event);
        const bool ctrl = ke->modifiers().testFlag(Qt::ControlModifier);

        switch (ke->key()) {
        case Qt::Key_Down:
            moveSelection(1);
            return true;
        case Qt::Key_Up:
            moveSelection(-1);
            return true;
        case Qt::Key_N:
            if (ctrl) {
                moveSelection(1);
                return true;
            }
            break;
        case Qt::Key_P:
            if (ctrl) {
                moveSelection(-1);
                return true;
            }
            break;
        case Qt::Key_PageDown:
            moveSelection(8);
            return true;
        case Qt::Key_PageUp:
            moveSelection(-8);
            return true;
        case Qt::Key_Enter:
        case Qt::Key_Return:
            triggerCurrent();
            return true;
        case Qt::Key_Escape:
            reject();
            return true;
        default:
            break;
        }
    }
    return QDialog::eventFilter(watched, event);
}
