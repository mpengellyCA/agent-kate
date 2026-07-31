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

#include "EnsembleDialog.h"
#include "state/EnsembleCatalog.h"

#include <QComboBox>
#include <QJsonObject>
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
};

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
