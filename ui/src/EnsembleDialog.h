// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include "state/EnsembleCatalog.h"

#include <QDialog>
#include <QList>

class CoreClient;
class QComboBox;
class QLabel;
class QLineEdit;
class QListWidget;
class QPlainTextEdit;
class QPushButton;
class QVBoxLayout;
class QWidget;

// EnsembleDialog edits the ensemble catalogue: pick one on the left, edit its
// controller, worker roster and master prompt on the right, save or delete.
//
// It edits a LOCAL copy and writes through mode.save on Save, so switching
// away without saving discards the edit rather than half-writing it. Built-in
// ensembles are editable too — saving one shadows it core-side, and deleting
// the shadow reveals the original again.
class EnsembleDialog : public QDialog
{
    Q_OBJECT
public:
    explicit EnsembleDialog(CoreClient *core, QWidget *parent = nullptr);

    // The model id an editable model combo currently expresses. Public and
    // static because it is the one rule in this dialog that can silently LOSE
    // data — an id the local catalogue has never seen is shown as edit text
    // while the index still points at "Engine default" — so it is tested
    // directly (ui/tests/EnsembleDialogTest).
    static QString modelIdFor(const QComboBox *combo);

private:
    // One editable worker row: role, engine, model and a note, plus its Remove
    // button. Held by value in m_workerRows so the read-back is a simple loop.
    struct WorkerRow {
        QWidget *widget = nullptr;
        QLineEdit *role = nullptr;
        QComboBox *backend = nullptr;
        QComboBox *model = nullptr;
        QComboBox *isolation = nullptr;
        QLineEdit *notes = nullptr;
    };

    void reloadList(const QString &selectName = QString());
    void showEnsemble(const Ensemble &e);
    Ensemble collect() const; // the edited copy, from the widgets
    void addWorkerRow(const EnsembleMember &w);
    void clearWorkerRows();
    // Fill a model combo with the engine's live catalogue, keeping `keep`
    // selected (or as free text, since a model id may predate the catalogue).
    void fillModels(QComboBox *models, const QString &backend, const QString &keep);
    void fillBackends(QComboBox *backends, const QString &keep);
    void onSave();
    void onDelete();

    CoreClient *m_core = nullptr;
    QListWidget *m_list = nullptr;
    QLineEdit *m_name = nullptr;
    QLineEdit *m_description = nullptr;
    QComboBox *m_controllerBackend = nullptr;
    QComboBox *m_controllerModel = nullptr;
    QComboBox *m_controllerIsolation = nullptr;
    QVBoxLayout *m_workerBox = nullptr;
    QList<WorkerRow> m_workerRows;
    QPlainTextEdit *m_prompt = nullptr;
    QLabel *m_status = nullptr;
    QPushButton *m_deleteButton = nullptr;
    bool m_loading = false; // suppress list-selection churn while repopulating
};
