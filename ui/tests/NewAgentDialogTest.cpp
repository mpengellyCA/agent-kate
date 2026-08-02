// Audit F49 / F30 — the guided New Agent dialog's isolation control.
//
// F49: the recommended default (the checkbox is on) mapped to a hard
// "isolated", and worktree.Create refuses that on a repo with no commit to
// branch from ("isolation needs at least one commit"), so starting an agent in
// a brand-new project failed with raw git-speak in the conversation. "auto"
// isolates where isolation is possible and degrades — visibly, the panel says
// "Working directly in your files" — where it is not.
//
// F30: the control must not call a git worktree a "sandbox". It isolates
// checkout state, not the process; docs/security-model.md says in bold that it
// is not a sandbox and does not pretend to be one. This dialog's wording is the
// canonical honest form the rest of the UI is aligned to, so it is pinned here.

#include "NewAgentDialog.h"

#include <QCheckBox>
#include <QtTest>

namespace {
// The isolation checkbox is the dialog's only checkbox outside the Advanced
// section, and it is the one whose text names a private copy.
QCheckBox *isolationBox(QWidget *dlg)
{
    const auto boxes = dlg->findChildren<QCheckBox *>();
    for (QCheckBox *b : boxes) {
        if (b->text().contains(QStringLiteral("private copy"))) {
            return b;
        }
    }
    return nullptr;
}
} // namespace

class NewAgentDialogTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void recommendedDefaultRequestsAuto();
    void uncheckedRequestsWorkspace();
    void isolationCopyNeverSaysSandbox();
};

// The recommended default must be a mode that can succeed on ANY project,
// including one with no commits. "isolated" is not that mode.
void NewAgentDialogTest::recommendedDefaultRequestsAuto()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    QVERIFY2(box->isChecked(), "the private-copy default must stay on");
    QCOMPARE(dlg.choices().isolation, QStringLiteral("auto"));
}

void NewAgentDialogTest::uncheckedRequestsWorkspace()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    box->setChecked(false);
    QCOMPARE(dlg.choices().isolation, QStringLiteral("workspace"));
}

// A worktree is not a sandbox. The label and its tooltip are the decision
// point, so neither may teach the containment belief the mechanism does not
// deliver — and the tooltip must say what it actually does instead.
void NewAgentDialogTest::isolationCopyNeverSaysSandbox()
{
    NewAgentDialog dlg(QStringLiteral("proj"), nullptr);
    QCheckBox *box = isolationBox(&dlg);
    QVERIFY(box != nullptr);
    QVERIFY(!box->text().contains(QStringLiteral("sandbox"), Qt::CaseInsensitive));
    const QString tip = box->toolTip();
    QVERIFY(!tip.isEmpty());
    QVERIFY(tip.contains(QStringLiteral("not a security sandbox")));
    QVERIFY(tip.contains(QStringLiteral("git worktree")));
    // The "sandbox" the tooltip may still mention is the one it denies.
    QVERIFY(!tip.contains(QStringLiteral("own sandbox")));
}

QTEST_MAIN(NewAgentDialogTest)
#include "NewAgentDialogTest.moc"
