#pragma once

#include <QDialog>

class CoreClient;
class QLabel;
class QProgressBar;
class QJsonObject;

// ShutdownDialog drives a graceful, observable app shutdown. It asks the core
// to stop and compact every agent (app.shutdown) and renders the streamed
// shutdown.progress events — "Compacting agent context for resume (i of N)",
// "Stopping agents", "Shutting down file watchers" — until the core reports
// "done" (or the process disconnects). Run it modally with exec() from
// MainWindow::closeEvent; it cannot be dismissed, so shutdown always completes
// and every agent is left resumable.
class ShutdownDialog : public QDialog
{
    Q_OBJECT
public:
    explicit ShutdownDialog(CoreClient *core, QWidget *parent = nullptr);

private:
    void onProgress(const QString &method, const QJsonObject &params);

    CoreClient *m_core = nullptr;
    QLabel *m_status = nullptr;
    QProgressBar *m_bar = nullptr;
};
