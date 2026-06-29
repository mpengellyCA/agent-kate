// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QDialog>
#include <QString>

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
// the right pane previews the selection. Selecting a theme applies it live to the
// whole running app (without persisting) so the user sees the real result. The
// choice is only persisted on Ok/Apply; Cancel restores the theme that was active
// when the dialog opened.
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
    void onSelectionChanged();
    void updatePreview(const QString &id);
    QString selectedId() const;
    void revertToOriginal();

    QString m_originalId; // theme active when the dialog opened (for revert)

    QListWidget *m_list = nullptr;

    QLabel *m_previewName = nullptr;
    QLabel *m_previewDesc = nullptr;
    QLabel *m_previewChip = nullptr;
    QGridLayout *m_swatchGrid = nullptr;
    QPushButton *m_sampleButton = nullptr;
    QLabel *m_sampleSelectedText = nullptr;
};
