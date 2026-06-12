#include "KPartView.h"
#include "ImageView.h"

#include <KLocalizedString>
#include <KPluginMetaData>
#include <KParts/PartLoader>
#include <KParts/ReadOnlyPart>

#include <QDesktopServices>
#include <QFileInfo>
#include <QHBoxLayout>
#include <QIcon>
#include <QLabel>
#include <QList>
#include <QMimeDatabase>
#include <QMimeType>
#include <QPushButton>
#include <QUrl>
#include <QVBoxLayout>

#include <algorithm>
#include <utility>

namespace {

// Archive parts (Ark) over-claim zip-based document formats: .odt/.docx/.epub
// are all technically zip containers, so Ark turns up as a candidate alongside
// the real document viewer (Okular). Push archive parts to the back of the
// candidate list so a genuine viewer wins for documents, while pure archives
// (.zip/.tar, where Ark is the sole candidate) still open in Ark.
bool isArchivePart(const KPluginMetaData &part)
{
    return part.pluginId().contains(QLatin1String("ark"));
}

QString describe(const QMimeType &mime, const QString &fallbackName)
{
    return mime.isValid() && !mime.comment().isEmpty() ? mime.comment() : fallbackName;
}

} // namespace

bool KPartView::canDisplay(const QString &path)
{
    QMimeDatabase db;
    const QMimeType mime = db.mimeTypeForFile(path);
    if (!mime.isValid()) {
        return false;
    }
    // Types with a dedicated native view are dispatched ahead of KPartView in
    // EditorArea; never let a generic part claim them. text/markdown and
    // text/csv both inherit text/plain, so this single check also keeps
    // Markdown, CSV and every source file out of Okular (it advertises
    // text/plain) and routed to KTextEditor/MarkdownView/CsvView instead.
    if (mime.inherits(QStringLiteral("text/plain"))) {
        return false;
    }
    // Plain raster images belong to ImageView (dispatched first); formats it
    // can't decode (DjVu, multi-page documents) still fall through to a part.
    if (ImageView::canDisplay(path)) {
        return false;
    }
    return !KParts::PartLoader::partsForMimeType(mime.name()).isEmpty();
}

KPartView::KPartView(const QString &path, QWidget *parent)
    : QWidget(parent)
    , m_path(QFileInfo(path).absoluteFilePath())
{
    m_layout = new QVBoxLayout(this);
    m_layout->setContentsMargins(0, 0, 0, 0);

    QMimeDatabase db;
    const QMimeType mime = db.mimeTypeForFile(m_path);
    const QString mimeName =
        mime.isValid() ? mime.name() : QStringLiteral("application/octet-stream");

    QList<KPluginMetaData> candidates = KParts::PartLoader::partsForMimeType(mimeName);
    if (candidates.isEmpty()) {
        showFallback(i18n("No viewer is installed for %1.", describe(mime, mimeName)));
        return;
    }
    // Prefer a real document viewer over an archive lister for zip-based docs.
    std::stable_partition(candidates.begin(), candidates.end(),
                          [](const KPluginMetaData &p) { return !isArchivePart(p); });

    for (const KPluginMetaData &candidate : std::as_const(candidates)) {
        const auto result =
            KParts::PartLoader::instantiatePart<KParts::ReadOnlyPart>(candidate, this, this);
        if (result && result.plugin->widget()) {
            m_part = result.plugin;
            break;
        }
        delete result.plugin; // null-safe; reclaim a part that loaded without a widget
    }

    if (!m_part) {
        showFallback(i18n("The viewer for %1 could not be loaded.", describe(mime, mimeName)));
        return;
    }

    m_layout->addWidget(m_part->widget());
    m_part->openUrl(QUrl::fromLocalFile(m_path));
}

KPartView::~KPartView()
{
    // The part owns its widget; delete it explicitly (and first) so its teardown
    // runs while our layout is still intact — the same ownership contract as
    // TerminalPanel's deleteLater() of the Konsole container. Qt would also
    // destroy it as a child, but doing it here keeps part->widget() torn down
    // under the part rather than racing the QWidget child destruction order.
    delete m_part;
    m_part = nullptr;
}

void KPartView::showFallback(const QString &message)
{
    auto *container = new QWidget(this);
    auto *vbox = new QVBoxLayout(container);
    vbox->addStretch();

    auto *label = new QLabel(message, container);
    label->setAlignment(Qt::AlignCenter);
    label->setWordWrap(true);
    label->setStyleSheet(QStringLiteral("color: palette(mid);"));
    vbox->addWidget(label);

    auto *openButton = new QPushButton(QIcon::fromTheme(QStringLiteral("document-open")),
                                       i18n("Open Externally"), container);
    connect(openButton, &QPushButton::clicked, this,
            [this] { QDesktopServices::openUrl(QUrl::fromLocalFile(m_path)); });
    auto *buttonRow = new QHBoxLayout;
    buttonRow->addStretch();
    buttonRow->addWidget(openButton);
    buttonRow->addStretch();
    vbox->addLayout(buttonRow);

    vbox->addStretch();
    m_layout->addWidget(container);
}
