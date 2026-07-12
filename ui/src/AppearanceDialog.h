// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>

class QComboBox;
class QGridLayout;
class QLabel;
class QListWidget;
class QPushButton;

// AppearanceDialog — pick the look Agent Kate wears.
//
// Agent Kate keeps its own appearance separate from the rest of the desktop:
// it can show a built-in Agent Kate theme, follow the system colours, or borrow
// any installed KDE colour scheme — without the user touching System Settings.
//
// The left list offers every theme from ThemeManager (grouped with header rows);
// the right pane previews the selection. A combo box below picks the editor
// syntax-highlighting theme independently — "Match interface" to follow the
// palette above, or any specific KSyntaxHighlighting theme. Both selections apply
// live to the running app (without persisting) so the user sees the real result;
// they are only persisted on Ok/Apply. Cancel restores what was active on open.
class AppearanceDialog : public QDialog
{
    Q_OBJECT
public:
    explicit AppearanceDialog(QWidget *parent = nullptr);

    // Override: live preview mutated the running app, so a cancel must restore
    // the theme that was active when the dialog opened.
    void reject() override;

protected:
    void closeEvent(QCloseEvent *event) override;

private:
    void buildThemeList();
    void buildEditorThemeCombo();
    void buildTerminalProfileCombo();
    void onSelectionChanged();
    void onEditorThemeChanged();
    void onTerminalProfileChanged();
    void updatePreview(const QString &id);
    QString selectedId() const;
    QString selectedEditorThemeId() const;
    QString selectedTerminalProfileId() const;
    void revertToOriginal();

    QString m_originalId;              // interface theme active when opened (revert)
    QString m_originalEditorThemeId;   // editor theme active when opened (revert)
    QString m_originalTerminalProfile; // terminal profile active when opened (revert)

    QListWidget *m_list = nullptr;
    QComboBox *m_editorThemeCombo = nullptr;
    QComboBox *m_terminalProfileCombo = nullptr;

    QLabel *m_previewName = nullptr;
    QLabel *m_previewDesc = nullptr;
    QLabel *m_previewChip = nullptr;
    QGridLayout *m_swatchGrid = nullptr;
    QPushButton *m_sampleButton = nullptr;
    QLabel *m_sampleSelectedText = nullptr;
};
