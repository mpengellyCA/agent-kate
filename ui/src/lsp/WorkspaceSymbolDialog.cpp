#include "WorkspaceSymbolDialog.h"
#include "LspManager.h"

#include <KLocalizedString>

#include <QFileInfo>
#include <QIcon>
#include <QLineEdit>
#include <QListWidget>
#include <QListWidgetItem>
#include <QPointer>
#include <QTimer>
#include <QVBoxLayout>

namespace {
// LSP SymbolKind → a Breeze theme icon name.
QString iconForKind(int kind)
{
    switch (kind) {
    case 5:  // Class
    case 23: // Struct
    case 11: // Interface
        return QStringLiteral("code-class");
    case 6:  // Method
    case 12: // Function
    case 9:  // Constructor
        return QStringLiteral("code-function");
    case 8:  // Field
    case 7:  // Property
    case 13: // Variable
    case 14: // Constant
        return QStringLiteral("code-variable");
    case 2:  // Module
    case 3:  // Namespace
    case 4:  // Package
        return QStringLiteral("code-context");
    default:
        return QStringLiteral("code-context");
    }
}
} // namespace

WorkspaceSymbolDialog::WorkspaceSymbolDialog(LspManager *lsp, QWidget *parent)
    : QDialog(parent)
    , m_lsp(lsp)
    , m_query(new QLineEdit(this))
    , m_list(new QListWidget(this))
    , m_debounce(new QTimer(this))
{
    setWindowTitle(i18nc("@title:window", "Go to Symbol in Workspace"));
    resize(560, 420);

    m_query->setPlaceholderText(i18n("Type a symbol name…"));
    m_query->setClearButtonEnabled(true);
    m_list->setFrameShape(QFrame::NoFrame);

    auto *layout = new QVBoxLayout(this);
    layout->addWidget(m_query);
    layout->addWidget(m_list);

    m_debounce->setSingleShot(true);
    m_debounce->setInterval(180);
    connect(m_debounce, &QTimer::timeout, this, &WorkspaceSymbolDialog::runQuery);
    connect(m_query, &QLineEdit::textChanged, this, [this] { m_debounce->start(); });

    // Enter in the query box activates the first result; Down moves into the list.
    connect(m_query, &QLineEdit::returnPressed, this, [this] {
        if (m_list->count() > 0) {
            m_list->setCurrentRow(0);
            Q_EMIT m_list->itemActivated(m_list->currentItem());
        }
    });
    connect(m_list, &QListWidget::itemActivated, this, [this](QListWidgetItem *item) {
        const QString path = item->data(Qt::UserRole).toString();
        if (!path.isEmpty()) {
            emit symbolChosen(path, item->data(Qt::UserRole + 1).toInt());
            accept();
        }
    });

    m_query->setFocus();
}

void WorkspaceSymbolDialog::runQuery()
{
    const QString q = m_query->text().trimmed();
    if (q.isEmpty()) {
        m_list->clear();
        return;
    }
    QPointer<WorkspaceSymbolDialog> self(this);
    m_lsp->requestWorkspaceSymbols(q, [self](const QList<Symbol> &symbols) {
        if (self) {
            self->populate(symbols);
        }
    });
}

void WorkspaceSymbolDialog::populate(const QList<Symbol> &symbols)
{
    m_list->clear();
    for (const Symbol &s : symbols) {
        QString label = s.name;
        if (!s.detail.isEmpty()) {
            label += QStringLiteral("  —  ") + s.detail;
        }
        if (!s.path.isEmpty()) {
            label += QStringLiteral("   (%1)").arg(QFileInfo(s.path).fileName());
        }
        auto *item = new QListWidgetItem(QIcon::fromTheme(iconForKind(s.kind)), label);
        item->setData(Qt::UserRole, s.path);
        item->setData(Qt::UserRole + 1, s.line);
        item->setToolTip(s.path);
        m_list->addItem(item);
    }
}
