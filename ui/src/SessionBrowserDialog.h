#pragma once

#include <QDialog>

class CoreClient;
class QComboBox;
class QJsonArray;
class QLabel;
class QLineEdit;
class QListWidget;
class QListWidgetItem;
class QPoint;
class QPushButton;
class QSplitter;
class QTextBrowser;
class QTimer;

// SessionBrowserDialog lists every Claude Code session found on disk and lets
// the user attach one — pulling that conversation into Agent Kate as a thread,
// even if Agent Kate did not start it. Backed by the core's session.browse and
// session.attach IPC.
class SessionBrowserDialog : public QDialog
{
    Q_OBJECT
public:
    explicit SessionBrowserDialog(CoreClient *core, QWidget *parent = nullptr);
    ~SessionBrowserDialog() override;

Q_SIGNALS:
    // Emitted once a session has been attached as thread threadId.
    void attachRequested(const QString &project, const QString &threadId,
                         const QString &title);

private:
    void refresh();
    void populate(const QJsonArray &sessions);
    void applyFilter();
    void applySort();
    void attachSelected();
    void forgetSelected();
    void loadPreview();
    void renderPreview(const QJsonArray &messages, bool truncated);
    void showContextMenu(const QPoint &pos);
    void updateActionButtons();

    CoreClient *m_core = nullptr;
    QLineEdit *m_search = nullptr;
    QComboBox *m_sort = nullptr;
    QSplitter *m_splitter = nullptr;
    QListWidget *m_list = nullptr;
    QTextBrowser *m_preview = nullptr;
    QTimer *m_previewTimer = nullptr;
    QLabel *m_status = nullptr;
    QPushButton *m_attachButton = nullptr;
    QPushButton *m_forgetButton = nullptr;
    QString m_previewSessionId; // id whose preview is currently shown/loading
};
