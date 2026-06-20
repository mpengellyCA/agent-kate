#include "LspActions.h"
#include "LspManager.h"

#include <KLocalizedString>

#include <QAction>
#include <QCursor>
#include <QIcon>
#include <QJsonObject>
#include <QMenu>
#include <QPoint>
#include <QWidget>

namespace LspActions {

void showMenu(LspManager *manager, LspClient *client, const QJsonArray &actions,
              QWidget *parent, const QPoint &at)
{
    if (!manager || !client) {
        return;
    }
    auto *menu = new QMenu(parent);
    menu->setAttribute(Qt::WA_DeleteOnClose);
    const QIcon icon = QIcon::fromTheme(QStringLiteral("tools-wizard"));

    if (actions.isEmpty()) {
        QAction *none = menu->addAction(i18n("No quick fixes available"));
        none->setEnabled(false);
    }

    for (const QJsonValue &v : actions) {
        const QJsonObject a = v.toObject();
        const QString title = a.value(QStringLiteral("title")).toString();
        if (title.isEmpty()) {
            continue;
        }
        QAction *act = menu->addAction(icon, title);
        QObject::connect(act, &QAction::triggered, menu,
                         [manager, client, a] { manager->executeCodeAction(client, a); });
    }

    const QPoint pos = at.isNull() ? QCursor::pos() : at;
    menu->popup(pos);
}

} // namespace LspActions
