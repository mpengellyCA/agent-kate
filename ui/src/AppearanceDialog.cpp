// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "AppearanceDialog.h"

#include "theme/ThemeManager.h"

#include <KLocalizedString>

#include <QCloseEvent>
#include <QColor>
#include <QComboBox>
#include <QDialogButtonBox>
#include <QFrame>
#include <QGridLayout>
#include <QHBoxLayout>
#include <QIcon>
#include <QLabel>
#include <QListWidget>
#include <QListWidgetItem>
#include <QPainter>
#include <QPixmap>
#include <QPushButton>
#include <QVBoxLayout>

namespace {

// Data roles on the theme rows.
constexpr int IdRole = Qt::UserRole;        // QString theme id (empty for headers)
constexpr int HeaderRole = Qt::UserRole + 1; // bool: true for non-selectable headers

// A tiny swatch the user can read at a glance: a horizontal strip split into the
// base background, accent, positive and negative colours of a theme. Painted by
// hand so it works for every theme kind (built-in, follow-system, KDE scheme).
QIcon swatchIcon(const AkThemeDef &def)
{
    const int w = 32;
    const int h = 18;
    QPixmap pm(w, h);
    pm.fill(Qt::transparent);

    QPainter p(&pm);
    p.setRenderHint(QPainter::Antialiasing, false);

    const QColor base = def.palette.color(QPalette::Base);
    const QColor accent = def.colors.accent;
    const QColor positive = def.colors.positive;
    const QColor negative = def.colors.negative;

    // Left two-thirds is the base background; the remaining third is three thin
    // accent/positive/negative stripes.
    const int split = (w * 2) / 3;
    p.fillRect(QRect(0, 0, split, h), base.isValid() ? base : QColor(40, 40, 40));

    const int stripeX = split;
    const int stripeW = w - split;
    const int third = h / 3;
    p.fillRect(QRect(stripeX, 0, stripeW, third),
               accent.isValid() ? accent : QColor(80, 120, 200));
    p.fillRect(QRect(stripeX, third, stripeW, third),
               positive.isValid() ? positive : QColor(80, 180, 80));
    p.fillRect(QRect(stripeX, 2 * third, stripeW, h - 2 * third),
               negative.isValid() ? negative : QColor(200, 80, 80));

    // A subtle 1px frame so light swatches still read on a light list.
    p.setPen(QColor(0, 0, 0, 90));
    p.drawRect(QRect(0, 0, w - 1, h - 1));
    p.end();

    return QIcon(pm);
}

// A non-selectable group header row.
QListWidgetItem *makeHeader(const QString &text)
{
    auto *item = new QListWidgetItem(text);
    item->setData(HeaderRole, true);
    item->setFlags(Qt::NoItemFlags); // not selectable, not enabled
    QFont f = item->font();
    f.setBold(true);
    item->setFont(f);
    return item;
}

// A small flat colour chip used in the preview swatch row.
QLabel *makeColorChip(const QColor &color, QWidget *parent)
{
    auto *chip = new QLabel(parent);
    chip->setFixedSize(28, 28);
    chip->setAutoFillBackground(false);
    const QColor c = color.isValid() ? color : QColor(Qt::gray);
    chip->setStyleSheet(
        QStringLiteral("background:%1; border:1px solid rgba(0,0,0,0.35); "
                       "border-radius:4px;")
            .arg(c.name(QColor::HexRgb)));
    chip->setToolTip(c.name(QColor::HexRgb));
    return chip;
}

} // namespace

AppearanceDialog::AppearanceDialog(QWidget *parent)
    : QDialog(parent)
{
    setObjectName(QStringLiteral("AppearanceDialog"));
    setWindowTitle(i18n("Appearance"));
    resize(640, 460);

    m_originalId = ThemeManager::instance()->currentId();
    m_originalEditorThemeId = ThemeManager::instance()->editorThemeId();
    m_originalTerminalProfile = ThemeManager::instance()->terminalProfileId();

    auto *outer = new QVBoxLayout(this);

    auto *intro = new QLabel(
        i18n("Give Agent Kate its own look — independent of the rest of your "
             "desktop. Pick a built-in Agent Kate theme, follow your system "
             "colors, or borrow any installed KDE color scheme."),
        this);
    intro->setWordWrap(true);
    outer->addWidget(intro);

    auto *body = new QHBoxLayout;
    outer->addLayout(body, 1);

    // --- Left: the theme list (grouped, with swatch icons) ---
    m_list = new QListWidget(this);
    m_list->setIconSize(QSize(32, 18));
    m_list->setMinimumWidth(200);
    body->addWidget(m_list, 1);

    // --- Right: preview & details pane ---
    auto *right = new QVBoxLayout;
    body->addLayout(right, 2);

    m_previewName = new QLabel(this);
    QFont nameFont = m_previewName->font();
    nameFont.setPointSizeF(nameFont.pointSizeF() * 1.3);
    nameFont.setBold(true);
    m_previewName->setFont(nameFont);
    right->addWidget(m_previewName);

    m_previewChip = new QLabel(this);
    m_previewChip->setTextFormat(Qt::PlainText);
    right->addWidget(m_previewChip);

    m_previewDesc = new QLabel(this);
    m_previewDesc->setWordWrap(true);
    m_previewDesc->setTextFormat(Qt::PlainText);
    right->addWidget(m_previewDesc);

    auto *sep1 = new QFrame(this);
    sep1->setFrameShape(QFrame::HLine);
    right->addWidget(sep1);

    auto *swatchHeader = new QLabel(i18n("Colors"), this);
    right->addWidget(swatchHeader);

    // The swatch grid: a chip per semantic colour, with a tiny label beneath.
    m_swatchGrid = new QGridLayout;
    m_swatchGrid->setHorizontalSpacing(10);
    m_swatchGrid->setVerticalSpacing(2);
    right->addLayout(m_swatchGrid);

    auto *sep2 = new QFrame(this);
    sep2->setFrameShape(QFrame::HLine);
    right->addWidget(sep2);

    auto *sampleHeader = new QLabel(i18n("Sample"), this);
    right->addWidget(sampleHeader);

    auto *sampleRow = new QHBoxLayout;
    m_sampleButton = new QPushButton(i18n("Sample button"), this);
    sampleRow->addWidget(m_sampleButton);
    m_sampleSelectedText = new QLabel(i18n("Selected text"), this);
    m_sampleSelectedText->setAutoFillBackground(true);
    m_sampleSelectedText->setMargin(4);
    sampleRow->addWidget(m_sampleSelectedText);
    sampleRow->addStretch(1);
    right->addLayout(sampleRow);

    right->addStretch(1);

    // --- Editor syntax theme (independent of the interface palette) ---
    auto *editorSep = new QFrame(this);
    editorSep->setFrameShape(QFrame::HLine);
    outer->addWidget(editorSep);

    auto *editorRow = new QHBoxLayout;
    auto *editorLabel = new QLabel(i18n("Editor syntax theme:"), this);
    editorRow->addWidget(editorLabel);
    m_editorThemeCombo = new QComboBox(this);
    m_editorThemeCombo->setToolTip(
        i18n("The colour theme used to highlight code in the editor, diff and "
             "inspector — independent of the interface theme above."));
    editorLabel->setBuddy(m_editorThemeCombo);
    editorRow->addWidget(m_editorThemeCombo, 1);
    outer->addLayout(editorRow);

    auto *terminalRow = new QHBoxLayout;
    auto *terminalLabel = new QLabel(i18n("Terminal profile:"), this);
    terminalRow->addWidget(terminalLabel);
    m_terminalProfileCombo = new QComboBox(this);
    m_terminalProfileCombo->setToolTip(
        i18n("The Konsole profile (colours and behaviour) used by the integrated "
             "terminal. \"Match interface\" tracks the interface theme's light or "
             "dark variant."));
    terminalLabel->setBuddy(m_terminalProfileCombo);
    terminalRow->addWidget(m_terminalProfileCombo, 1);
    outer->addLayout(terminalRow);

    // --- Buttons ---
    auto *buttons = new QDialogButtonBox(QDialogButtonBox::Ok | QDialogButtonBox::Cancel |
                                             QDialogButtonBox::Apply,
                                         this);
    outer->addWidget(buttons);

    connect(buttons, &QDialogButtonBox::accepted, this, [this] {
        // Persist the live-previewed selections, then close.
        ThemeManager::instance()->applyTheme(selectedId(), /*persist=*/true);
        ThemeManager::instance()->setEditorTheme(selectedEditorThemeId(), /*persist=*/true);
        ThemeManager::instance()->setTerminalProfile(selectedTerminalProfileId(), /*persist=*/true);
        accept();
    });
    connect(buttons, &QDialogButtonBox::rejected, this, &AppearanceDialog::reject);
    connect(buttons->button(QDialogButtonBox::Apply), &QPushButton::clicked, this, [this] {
        // Persist but keep the dialog open.
        ThemeManager::instance()->applyTheme(selectedId(), /*persist=*/true);
        ThemeManager::instance()->setEditorTheme(selectedEditorThemeId(), /*persist=*/true);
        ThemeManager::instance()->setTerminalProfile(selectedTerminalProfileId(), /*persist=*/true);
    });

    connect(m_list, &QListWidget::currentRowChanged, this,
            [this](int) { onSelectionChanged(); });
    connect(m_editorThemeCombo, &QComboBox::currentIndexChanged, this,
            [this](int) { onEditorThemeChanged(); });
    connect(m_terminalProfileCombo, &QComboBox::currentIndexChanged, this,
            [this](int) { onTerminalProfileChanged(); });

    buildThemeList();
    buildEditorThemeCombo();
    buildTerminalProfileCombo();
}

void AppearanceDialog::buildThemeList()
{
    const QList<AkThemeDef> all = ThemeManager::instance()->themes();
    const QString current = ThemeManager::instance()->currentId();

    m_list->blockSignals(true);
    m_list->clear();

    bool addedBuiltinHeader = false;
    bool addedSystemHeader = false;
    bool addedKdeHeader = false;
    int rowToSelect = -1;

    for (const AkThemeDef &def : all) {
        // Emit the appropriate group header just before the first item of a kind.
        switch (def.kind) {
        case AkThemeDef::BuiltinPalette:
            if (!addedBuiltinHeader) {
                m_list->addItem(makeHeader(i18n("Agent Kate")));
                addedBuiltinHeader = true;
            }
            break;
        case AkThemeDef::FollowSystem:
            if (!addedSystemHeader) {
                m_list->addItem(makeHeader(i18n("System")));
                addedSystemHeader = true;
            }
            break;
        case AkThemeDef::KdeScheme:
            if (!addedKdeHeader) {
                m_list->addItem(makeHeader(i18n("Installed KDE schemes")));
                addedKdeHeader = true;
            }
            break;
        }

        auto *item = new QListWidgetItem(swatchIcon(def), def.name);
        item->setData(IdRole, def.id);
        item->setData(HeaderRole, false);
        item->setToolTip(def.description);
        m_list->addItem(item);

        if (def.id == current) {
            rowToSelect = m_list->count() - 1;
        }
    }

    m_list->blockSignals(false);

    if (rowToSelect >= 0) {
        m_list->setCurrentRow(rowToSelect);
    }
    // Refresh the preview for the (pre-)selected row even if no signal fired.
    onSelectionChanged();
}

void AppearanceDialog::buildEditorThemeCombo()
{
    // Row 0 is "Match interface" (empty id); the rest are concrete theme names,
    // whose id is the theme name itself.
    m_editorThemeCombo->blockSignals(true);
    m_editorThemeCombo->clear();
    m_editorThemeCombo->addItem(i18n("Match interface (automatic)"), QString());
    for (const QString &name : ThemeManager::availableEditorThemes()) {
        m_editorThemeCombo->addItem(name, name);
    }

    const QString current = ThemeManager::instance()->editorThemeId();
    int row = m_editorThemeCombo->findData(current);
    m_editorThemeCombo->setCurrentIndex(row >= 0 ? row : 0);
    m_editorThemeCombo->blockSignals(false);
}

QString AppearanceDialog::selectedEditorThemeId() const
{
    if (!m_editorThemeCombo) {
        return m_originalEditorThemeId;
    }
    return m_editorThemeCombo->currentData().toString();
}

void AppearanceDialog::onEditorThemeChanged()
{
    // LIVE PREVIEW: apply to the running app (editors re-theme) without persisting.
    ThemeManager::instance()->setEditorTheme(selectedEditorThemeId(), /*persist=*/false);
}

void AppearanceDialog::buildTerminalProfileCombo()
{
    // Row 0 is "Match interface" (empty id); the rest are Konsole profile names,
    // whose id is the profile name itself.
    m_terminalProfileCombo->blockSignals(true);
    m_terminalProfileCombo->clear();
    m_terminalProfileCombo->addItem(i18n("Match interface (automatic)"), QString());
    for (const QString &name : ThemeManager::availableTerminalProfiles()) {
        m_terminalProfileCombo->addItem(name, name);
    }

    const QString current = ThemeManager::instance()->terminalProfileId();
    int row = m_terminalProfileCombo->findData(current);
    m_terminalProfileCombo->setCurrentIndex(row >= 0 ? row : 0);
    m_terminalProfileCombo->blockSignals(false);
}

QString AppearanceDialog::selectedTerminalProfileId() const
{
    if (!m_terminalProfileCombo) {
        return m_originalTerminalProfile;
    }
    return m_terminalProfileCombo->currentData().toString();
}

void AppearanceDialog::onTerminalProfileChanged()
{
    // LIVE PREVIEW: re-profile running terminal sessions without persisting.
    ThemeManager::instance()->setTerminalProfile(selectedTerminalProfileId(), /*persist=*/false);
}

QString AppearanceDialog::selectedId() const
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item) {
        return m_originalId;
    }
    const QString id = item->data(IdRole).toString();
    return id.isEmpty() ? m_originalId : id;
}

void AppearanceDialog::onSelectionChanged()
{
    QListWidgetItem *item = m_list->currentItem();
    if (!item || item->data(HeaderRole).toBool()) {
        return; // headers carry no theme
    }
    const QString id = item->data(IdRole).toString();
    if (id.isEmpty()) {
        return;
    }

    // LIVE PREVIEW: apply to the whole running app without persisting.
    ThemeManager::instance()->applyTheme(id, /*persist=*/false);
    updatePreview(id);
}

void AppearanceDialog::updatePreview(const QString &id)
{
    const AkThemeDef def = ThemeManager::instance()->themeById(id);

    m_previewName->setText(def.name);
    m_previewDesc->setText(def.description);
    m_previewChip->setText(def.dark ? i18n("Dark theme") : i18n("Light theme"));

    // Rebuild the swatch grid for this theme's semantic colours.
    while (QLayoutItem *child = m_swatchGrid->takeAt(0)) {
        if (QWidget *w = child->widget()) {
            w->deleteLater();
        }
        delete child;
    }

    const AkColors &c = def.colors;
    const QColor highlight = def.palette.color(QPalette::Highlight);

    struct Entry {
        QString label;
        QColor color;
    };
    const QList<Entry> entries = {
        {i18n("Accent"), c.accent},
        {i18n("Selection"), highlight},
        {i18n("Positive"), c.positive},
        {i18n("Negative"), c.negative},
        {i18n("Neutral"), c.neutral},
        {i18n("Info"), c.info},
    };

    int col = 0;
    for (const Entry &e : entries) {
        auto *chip = makeColorChip(e.color, this);
        auto *label = new QLabel(e.label, this);
        label->setAlignment(Qt::AlignHCenter);
        QFont lf = label->font();
        lf.setPointSizeF(qMax(6.0, lf.pointSizeF() - 1.0));
        label->setFont(lf);
        m_swatchGrid->addWidget(chip, 0, col, Qt::AlignHCenter);
        m_swatchGrid->addWidget(label, 1, col, Qt::AlignHCenter);
        ++col;
    }

    // Style the "selected text" sample to use this theme's highlight colours so
    // the user sees real selection styling rather than a static swatch.
    const QColor selBg = highlight.isValid() ? highlight : c.accent;
    const QColor selFg = def.palette.color(QPalette::HighlightedText);
    QPalette pal = m_sampleSelectedText->palette();
    if (selBg.isValid()) {
        pal.setColor(QPalette::Window, selBg);
    }
    pal.setColor(QPalette::WindowText, selFg.isValid() ? selFg : c.accentText);
    m_sampleSelectedText->setPalette(pal);
}

void AppearanceDialog::revertToOriginal()
{
    // Live preview changed the running app — put back what was there on open.
    if (ThemeManager::instance()->currentId() != m_originalId) {
        ThemeManager::instance()->applyTheme(m_originalId, /*persist=*/true);
    }
    if (ThemeManager::instance()->editorThemeId() != m_originalEditorThemeId) {
        ThemeManager::instance()->setEditorTheme(m_originalEditorThemeId, /*persist=*/true);
    }
    if (ThemeManager::instance()->terminalProfileId() != m_originalTerminalProfile) {
        ThemeManager::instance()->setTerminalProfile(m_originalTerminalProfile, /*persist=*/true);
    }
}

void AppearanceDialog::reject()
{
    revertToOriginal();
    QDialog::reject();
}

void AppearanceDialog::closeEvent(QCloseEvent *event)
{
    // Closing via the window manager [x] is a cancel: revert the live preview.
    revertToOriginal();
    QDialog::closeEvent(event);
}
