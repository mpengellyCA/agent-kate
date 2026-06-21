#include "ShutdownDialog.h"

#include "ipc/CoreClient.h"

#include <QJsonObject>
#include <QLabel>
#include <QProgressBar>
#include <QVBoxLayout>

#include <KLocalizedString>

ShutdownDialog::ShutdownDialog(CoreClient *core, QWidget *parent)
    : QDialog(parent)
    , m_core(core)
{
    setWindowTitle(i18n("Shutting Down — Agent Kate"));
    setModal(true);
    // The shutdown must run to completion so every agent is compacted and
    // resumable — drop the close button so it can't be dismissed mid-way.
    setWindowFlags((windowFlags() | Qt::CustomizeWindowHint)
                   & ~Qt::WindowCloseButtonHint);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(20, 18, 20, 18);
    layout->setSpacing(12);

    m_status = new QLabel(i18n("Preparing to stop agents…"), this);
    m_status->setWordWrap(true);
    layout->addWidget(m_status);

    m_bar = new QProgressBar(this);
    m_bar->setRange(0, 0); // indeterminate until a compaction count arrives
    m_bar->setTextVisible(false);
    layout->addWidget(m_bar);

    setMinimumWidth(400);

    connect(m_core, &CoreClient::notification, this, &ShutdownDialog::onProgress);
    // If the core dies before reporting "done", treat the disconnect as the end
    // of shutdown so the dialog can never wedge the quit.
    connect(m_core, &CoreClient::disconnected, this, &QDialog::accept);

    // Kick off the graceful shutdown. The core streams progress, then replies
    // ok once everything is drained; accept() on the reply as a belt-and-braces
    // companion to the "done" progress event.
    m_core->call(QStringLiteral("app.shutdown"), QJsonObject{},
                 [this](const QJsonObject &, const QJsonObject &) { accept(); });
}

void ShutdownDialog::onProgress(const QString &method, const QJsonObject &params)
{
    if (method != QLatin1String("shutdown.progress")) {
        return;
    }
    const QString phase = params.value(QStringLiteral("phase")).toString();
    const QString detail = params.value(QStringLiteral("detail")).toString();
    const int index = params.value(QStringLiteral("index")).toInt();
    const int total = params.value(QStringLiteral("total")).toInt();

    if (phase == QLatin1String("preparing")) {
        m_status->setText(i18np("Preparing to stop %1 agent…",
                                "Preparing to stop %1 agents…", total));
    } else if (phase == QLatin1String("compacting")) {
        m_bar->setRange(0, total);
        m_bar->setValue(index);
        m_status->setText(
            detail.isEmpty()
                ? i18n("Compacting agent context for resume (%1 of %2)…", index, total)
                : i18n("Compacting “%1” for resume (%2 of %3)…", detail, index, total));
    } else if (phase == QLatin1String("stopping")) {
        m_bar->setRange(0, 0);
        m_status->setText(i18n("Stopping agents…"));
    } else if (phase == QLatin1String("draining")) {
        m_bar->setRange(0, 0);
        m_status->setText(i18n("Finishing background compactions…"));
    } else if (phase == QLatin1String("watchers")) {
        m_status->setText(i18n("Shutting down file watchers…"));
    } else if (phase == QLatin1String("done")) {
        m_status->setText(i18n("Done."));
        accept();
    }
}
