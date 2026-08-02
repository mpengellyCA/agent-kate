#include "ProblemsPanel.h"
#include "lsp/LspManager.h"

#include <KLocalizedString>

#include <QAction>
#include <QFileInfo>
#include <QIcon>
#include <QMap>
#include <QToolBar>
#include <QTreeWidget>
#include <QTreeWidgetItem>
#include <QVBoxLayout>

namespace {
// Severity → a Breeze theme icon name so problems match the user's scheme.
QString severityIcon(int severity)
{
    switch (severity) {
    case 1:  return QStringLiteral("dialog-error");
    case 2:  return QStringLiteral("dialog-warning");
    default: return QStringLiteral("dialog-information");
    }
}
} // namespace

ProblemsPanel::ProblemsPanel(LspManager *lsp, QWidget *parent)
    : QWidget(parent)
    , m_lsp(lsp)
    , m_tree(new QTreeWidget(this))
{
    m_tree->setFrameShape(QFrame::NoFrame);
    m_tree->setHeaderHidden(true);
    m_tree->setIndentation(14);

    auto *toolbar = new QToolBar(this);
    toolbar->setToolButtonStyle(Qt::ToolButtonIconOnly);
    toolbar->setIconSize(QSize(16, 16));

    auto addFilter = [&](const QString &iconName, const QString &text) {
        auto *act = new QAction(QIcon::fromTheme(iconName), text, this);
        act->setCheckable(true);
        act->setChecked(true);
        toolbar->addAction(act);
        connect(act, &QAction::toggled, this, &ProblemsPanel::rebuild);
        return act;
    };
    m_showErrors = addFilter(QStringLiteral("dialog-error"), i18n("Show errors"));
    m_showWarnings = addFilter(QStringLiteral("dialog-warning"), i18n("Show warnings"));
    m_showInfo = addFilter(QStringLiteral("dialog-information"), i18n("Show information"));
    // Parented to `this`, so a destroyed panel takes them with it — which is
    // exactly what the palette's QPointer entries are guarding against.

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(toolbar);
    layout->addWidget(m_tree);

    connect(m_lsp, &LspManager::problemsChanged, this, &ProblemsPanel::rebuild);
    connect(m_tree, &QTreeWidget::itemActivated, this, [this](QTreeWidgetItem *item) {
        const QString path = item->data(0, Qt::UserRole).toString();
        if (!path.isEmpty()) {
            emit activated(path, item->data(0, Qt::UserRole + 1).toInt());
        }
    });

    rebuild();
}

QList<QAction *> ProblemsPanel::commands() const
{
    return {m_showErrors, m_showWarnings, m_showInfo};
}

void ProblemsPanel::rebuild()
{
    m_tree->clear();
    const QList<Problem> problems = m_lsp->problems();

    // Group surviving problems by file.
    QMap<QString, QList<Problem>> byFile;
    for (const Problem &p : problems) {
        const bool keep = (p.severity == 1 && m_showErrors->isChecked())
                          || (p.severity == 2 && m_showWarnings->isChecked())
                          || (p.severity >= 3 && m_showInfo->isChecked());
        if (keep) {
            byFile[p.path].append(p);
        }
    }

    if (byFile.isEmpty()) {
        auto *item = new QTreeWidgetItem(m_tree);
        item->setText(0, i18n("No problems detected"));
        item->setFlags(Qt::NoItemFlags);
        return;
    }

    for (auto it = byFile.constBegin(); it != byFile.constEnd(); ++it) {
        auto *header = new QTreeWidgetItem(m_tree);
        header->setText(0, QStringLiteral("%1  (%2)")
                               .arg(QFileInfo(it.key()).fileName())
                               .arg(it.value().size()));
        header->setIcon(0, QIcon::fromTheme(QStringLiteral("text-x-generic")));
        header->setToolTip(0, it.key());
        header->setFlags(Qt::ItemIsEnabled);
        header->setExpanded(true);

        for (const Problem &p : it.value()) {
            auto *child = new QTreeWidgetItem(header);
            child->setText(0, QStringLiteral("%1:  %2")
                                  .arg(p.line + 1)
                                  .arg(p.message.simplified()));
            child->setIcon(0, QIcon::fromTheme(severityIcon(p.severity)));
            child->setData(0, Qt::UserRole, p.path);
            child->setData(0, Qt::UserRole + 1, p.line);
            child->setToolTip(0, QStringLiteral("%1:%2").arg(p.path).arg(p.line + 1));
        }
    }
}
