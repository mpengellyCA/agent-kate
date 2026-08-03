// Plan 16 P4 guards for the two places an ensemble can silently lose data
// between the core and the editor:
//
//   1. the JSON round-trip (a worker's role/notes/isolation must survive
//      mode.get -> editor -> mode.save), and
//   2. the editable model combo, whose current INDEX lies: a model id this
//      machine has never discovered is shown as edit text while the index
//      still points at "Engine default", so reading the index would write an
//      ensemble with no model at all.
//
// Both are silent failures — the save succeeds, the ensemble is just wrong the
// next time it runs — which is exactly the kind a test has to catch.

// Audit F30/F49, the convergence round: this editor was one of the two pickers
// still calling "auto" a "Private copy (recommended)" after the guided dialog
// had stopped promising a copy it may not get. Its isolation labels must be the
// shared ones, and its engine list must carry the shared availability
// annotation (audit F37) — a recipe naming an engine this machine does not have
// is worth SAYING, even though an editor is right not to refuse the edit.

#include "EnsembleDialog.h"
#include "NewAgentDialog.h" // IsolationCopy — the shared isolation wording
#include "state/EngineAvailability.h"
#include "state/EnsembleCatalog.h"
#include "state/HarnessTraits.h"

#include <QComboBox>
#include <QDir>
#include <QFile>
#include <QJsonObject>
#include <QStandardPaths>
#include <QTemporaryDir>
#include <QtTest>

namespace {
// Populate a model combo the way EnsembleDialog::fillModels does.
void seedCatalogueCombo(QComboBox *combo)
{
    combo->setEditable(true);
    combo->addItem(QStringLiteral("Engine default"), QString());
    combo->addItem(QStringLiteral("Opus"), QStringLiteral("opus"));
    combo->addItem(QStringLiteral("K3"), QStringLiteral("kimi-code/k3"));
}
} // namespace

class EnsembleDialogTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void jsonRoundTripKeepsEveryField();
    void modelIdFromCatalogueEntry();
    void modelIdFromEngineDefault();
    void modelIdFromUnknownIdKeptAsTyped();
    void modelIdFromTypedText();
    void isolationLabelsComeFromTheSharedCopy();
    void engineListSaysWhichEnginesAreMissing();

private Q_SLOTS:
    void initTestCase();
    void cleanup();

private:
    QByteArray m_originalPath;
};

void EnsembleDialogTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    m_originalPath = qgetenv("PATH");
    HarnessTraits claude;
    claude.id = QStringLiteral("claude");
    claude.displayName = QStringLiteral("Claude Code");
    HarnessTraits kimi;
    kimi.id = QStringLiteral("kimi");
    kimi.displayName = QStringLiteral("Kimi Code");
    HarnessRegistry::self()->replaceDescriptorsForTest({claude, kimi});
}

void EnsembleDialogTest::cleanup()
{
    qputenv("PATH", m_originalPath);
    EngineAvailability::invalidate();
}

// The convergence itself. The editor's isolation combos must render
// IsolationCopy's words — not a local set that can drift back into promising a
// private copy "auto" may decline to give.
void EnsembleDialogTest::isolationLabelsComeFromTheSharedCopy()
{
    EnsembleDialog dlg(nullptr);
    QComboBox *isolation = nullptr;
    const auto combos = dlg.findChildren<QComboBox *>();
    for (QComboBox *c : combos) {
        if (c->findData(QStringLiteral("auto")) >= 0
            && c->findData(QStringLiteral("workspace")) >= 0) {
            isolation = c;
            break;
        }
    }
    QVERIFY2(isolation != nullptr, "the controller's isolation picker");
    for (const char *mode : {"auto", "isolated", "workspace"}) {
        const QString id = QString::fromLatin1(mode);
        const int idx = isolation->findData(id);
        QVERIFY(idx >= 0);
        QCOMPARE(isolation->itemText(idx), IsolationCopy::modeLabel(id));
    }
    // "auto" in particular: no unconditional promise anywhere in the editor.
    QCOMPARE(isolation->itemText(isolation->findData(QStringLiteral("auto"))),
             IsolationCopy::modeLabel(QStringLiteral("auto")));
    QVERIFY(!isolation->toolTip().isEmpty());
}

// Audit F37 in the editor: annotated, not disabled — an ensemble is a recipe
// saved now and run later, possibly after installing the engine.
void EnsembleDialogTest::engineListSaysWhichEnginesAreMissing()
{
    QTemporaryDir bin;
    QVERIFY(bin.isValid());
    qputenv("PATH", bin.path().toLocal8Bit()); // no engine CLI anywhere
    EngineAvailability::invalidate();

    EnsembleDialog dlg(nullptr);
    QComboBox *engines = nullptr;
    const auto combos = dlg.findChildren<QComboBox *>();
    for (QComboBox *c : combos) {
        if (c->findData(QStringLiteral("claude")) >= 0) {
            engines = c;
            break;
        }
    }
    qputenv("PATH", m_originalPath);
    EngineAvailability::invalidate();

    QVERIFY2(engines != nullptr, "the controller's engine picker");
    const int idx = engines->findData(QStringLiteral("claude"));
    QVERIFY(idx >= 0);
    if (!EngineAvailability::isPresent(QStringLiteral("claude"))) {
        QVERIFY2(engines->itemText(idx).contains(QStringLiteral("not installed")),
                 "a recipe naming an engine this machine lacks must say so");
    }
    QVERIFY2(engines->isEnabled(),
             "but the editor must still let the recipe be authored");
}

void EnsembleDialogTest::jsonRoundTripKeepsEveryField()
{
    Ensemble e;
    e.name = QStringLiteral("Crew");
    e.description = QStringLiteral("does things");
    e.controller = {QString(), QStringLiteral("claude"), QStringLiteral("fable"),
                    QStringLiteral("plan"), QStringLiteral("high"),
                    QStringLiteral("auto"), QString()};
    e.workers.append({QStringLiteral("coder"), QStringLiteral("kimi"),
                      QStringLiteral("kimi-code/k3"), QStringLiteral("yolo"),
                      QString(), QStringLiteral("workspace"),
                      QStringLiteral("writes the code")});
    e.masterPrompt = QStringLiteral("Lead {{ensemble_name}} in {{workspace}}.");

    const Ensemble back = EnsembleCatalog::fromJson(EnsembleCatalog::toJson(e));
    QCOMPARE(back.name, e.name);
    QCOMPARE(back.description, e.description);
    QCOMPARE(back.controller.backend, QStringLiteral("claude"));
    QCOMPARE(back.controller.model, QStringLiteral("fable"));
    QCOMPARE(back.controller.permissionMode, QStringLiteral("plan"));
    QCOMPARE(back.controller.effort, QStringLiteral("high"));
    QCOMPARE(back.controller.isolation, QStringLiteral("auto"));
    QCOMPARE(back.workers.size(), 1);
    QCOMPARE(back.workers.at(0).role, QStringLiteral("coder"));
    QCOMPARE(back.workers.at(0).model, QStringLiteral("kimi-code/k3"));
    QCOMPARE(back.workers.at(0).permissionMode, QStringLiteral("yolo"));
    QCOMPARE(back.workers.at(0).isolation, QStringLiteral("workspace"));
    QCOMPARE(back.workers.at(0).notes, QStringLiteral("writes the code"));
    QCOMPARE(back.masterPrompt, e.masterPrompt);
    QVERIFY2(back == e, "operator== does not see a field the round-trip carries");
}

void EnsembleDialogTest::modelIdFromCatalogueEntry()
{
    QComboBox combo;
    seedCatalogueCombo(&combo);
    combo.setCurrentIndex(combo.findData(QStringLiteral("opus")));
    QCOMPARE(EnsembleDialog::modelIdFor(&combo), QStringLiteral("opus"));
}

void EnsembleDialogTest::modelIdFromEngineDefault()
{
    QComboBox combo;
    seedCatalogueCombo(&combo);
    combo.setCurrentIndex(0);
    QVERIFY(EnsembleDialog::modelIdFor(&combo).isEmpty());
}

void EnsembleDialogTest::modelIdFromUnknownIdKeptAsTyped()
{
    // The regression: an ensemble written elsewhere (or before this machine
    // discovered the model) shows its id as edit text with the index still on
    // "Engine default". Reading the index would drop the model on save.
    QComboBox combo;
    seedCatalogueCombo(&combo);
    combo.setCurrentIndex(0);
    combo.setEditText(QStringLiteral("claude-opus-9"));
    QCOMPARE(EnsembleDialog::modelIdFor(&combo), QStringLiteral("claude-opus-9"));
}

void EnsembleDialogTest::modelIdFromTypedText()
{
    QComboBox combo;
    seedCatalogueCombo(&combo);
    combo.setCurrentIndex(combo.findData(QStringLiteral("opus")));
    combo.setEditText(QStringLiteral("  kimi-code/k3-256k  "));
    QCOMPARE(EnsembleDialog::modelIdFor(&combo), QStringLiteral("kimi-code/k3-256k"));
}

QTEST_MAIN(EnsembleDialogTest)
#include "EnsembleDialogTest.moc"
