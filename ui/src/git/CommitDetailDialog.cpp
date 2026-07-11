// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "CommitDetailDialog.h"
#include "DiffView.h"
#include "ipc/CoreClient.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KFormat>
#include <KLocalizedString>
#include <KSharedConfig>

#include <QApplication>
#include <QClipboard>
#include <QDateTime>
#include <QFont>
#include <QHBoxLayout>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QListWidget>
#include <QListWidgetItem>
#include <QPointer>
#include <QPushButton>
#include <QSplitter>
#include <QTabWidget>
#include <QVBoxLayout>

namespace {
QString statusGlyph(const QString &status)
{
    if (status == QLatin1String("modified")) return QStringLiteral("M");
    if (status == QLatin1String("added"))    return QStringLiteral("A");
    if (status == QLatin1String("deleted"))  return QStringLiteral("D");
    if (status == QLatin1String("renamed"))  return QStringLiteral("R");
    return QStringLiteral(" ");
}

// Semantic colour for a file's status glyph — added/deleted read at a glance.
QColor statusColor(const QString &status)
{
    const AkColors &c = ThemeManager::palette();
    if (status == QLatin1String("added"))   return c.positive;
    if (status == QLatin1String("deleted")) return c.negative;
    if (status == QLatin1String("renamed")) return c.info;
    return c.neutral; // modified / other
}

QString numstat(int added, int deleted)
{
    if (added < 0 || deleted < 0) {
        return i18nc("binary file in a commit's file list", "bin");
    }
    return QStringLiteral("+%1 −%2").arg(added).arg(deleted);
}

// Up to two initials from an author name, for the header chip ("Mike Pengelly"
// → "MP", "kate" → "K").
QString initials(const QString &name)
{
    const QStringList parts =
        name.split(QLatin1Char(' '), Qt::SkipEmptyParts);
    if (parts.isEmpty()) {
        return QStringLiteral("?");
    }
    QString out = parts.first().left(1).toUpper();
    if (parts.size() > 1) {
        out += parts.last().left(1).toUpper();
    }
    return out;
}

// A small coloured inline chip in the RichText header. `bg`/`fg` are hex.
QString htmlChip(const QString &text, const QColor &bg, const QColor &fg)
{
    return QStringLiteral(
               "<span style='background-color:%1;color:%2;"
               "padding:1px 5px;border-radius:4px;white-space:nowrap;'>%3</span>")
        .arg(bg.name(), fg.name(), text.toHtmlEscaped());
}

// Classify a ref label the way RefChipDelegate does, for a matching header chip.
QString refChipHtml(const QString &raw, const QPalette &pal)
{
    const AkColors &ak = ThemeManager::palette();
    QColor bg;
    QColor fg = pal.color(QPalette::HighlightedText);
    QString label = raw;
    if (raw.startsWith(QLatin1String("tag:"))) {
        label = raw.mid(4);
        bg = ak.neutral;
    } else if (raw.contains(QLatin1Char('/'))) {
        bg = pal.color(QPalette::Highlight).darker(130);
    } else {
        bg = pal.color(QPalette::Highlight);
    }
    return htmlChip(label, bg, fg);
}
} // namespace

CommitDetailDialog::CommitDetailDialog(CoreClient *core, const QString &threadId,
                                       const QString &repoRoot, const QString &sha,
                                       QWidget *parent)
    : QDialog(parent)
    , m_core(core)
    , m_threadId(threadId)
    , m_repoRoot(repoRoot)
    , m_sha(sha)
{
    setWindowTitle(i18nc("@title:window", "Commit %1", sha.left(8)));
    setAttribute(Qt::WA_DeleteOnClose);
    // Non-modal so the user can keep browsing the log with the dialog open.
    setModal(false);

    m_header = new QLabel(this);
    m_header->setTextFormat(Qt::RichText);
    m_header->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_header->setWordWrap(true);

    m_body = new QLabel(this);
    m_body->setTextFormat(Qt::PlainText);
    m_body->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_body->setWordWrap(true);
    {
        QFont mono = m_body->font();
        mono.setFamily(QStringLiteral("monospace"));
        mono.setStyleHint(QFont::TypeWriter);
        m_body->setFont(mono);
        QPalette pal = m_body->palette();
        pal.setColor(QPalette::WindowText,
                     pal.color(QPalette::Disabled, QPalette::WindowText));
        m_body->setPalette(pal);
    }

    // --- Changes tab: file list + scoped per-file diff -----------------------
    m_files = new QListWidget(this);
    m_files->setAlternatingRowColors(true);
    m_files->setSelectionMode(QAbstractItemView::SingleSelection);
    connect(m_files, &QListWidget::currentRowChanged, this,
            &CommitDetailDialog::onFileRowChanged);

    auto *changesDiffHost = new QWidget(this);
    m_changesDiffSlot = new QVBoxLayout(changesDiffHost);
    m_changesDiffSlot->setContentsMargins(0, 0, 0, 0);

    auto *changesSplit = new QSplitter(Qt::Horizontal, this);
    changesSplit->addWidget(m_files);
    changesSplit->addWidget(changesDiffHost);
    changesSplit->setStretchFactor(0, 1);
    changesSplit->setStretchFactor(1, 3);

    // --- Patch tab: whole-commit unified diff --------------------------------
    auto *patchHost = new QWidget(this);
    m_patchSlot = new QVBoxLayout(patchHost);
    m_patchSlot->setContentsMargins(0, 0, 0, 0);

    auto *tabs = new QTabWidget(this);
    tabs->addTab(changesSplit, i18nc("@title:tab", "Changes"));
    tabs->addTab(patchHost, i18nc("@title:tab", "Patch"));

    auto *copyShaBtn =
        new QPushButton(i18nc("@action:button", "Copy Hash"), this);
    connect(copyShaBtn, &QPushButton::clicked, this,
            [this] { QApplication::clipboard()->setText(m_sha); });
    auto *close = new QPushButton(i18nc("@action:button", "Close"), this);
    connect(close, &QPushButton::clicked, this, &QDialog::accept);
    auto *btnRow = new QHBoxLayout;
    btnRow->addWidget(copyShaBtn);
    btnRow->addStretch(1);
    btnRow->addWidget(close);

    auto *root = new QVBoxLayout(this);
    root->addWidget(m_header);
    root->addWidget(m_body);
    root->addWidget(tabs, 1);
    root->addLayout(btnRow);

    // Seed both diff panes so the tabs are never empty before the RPC returns.
    replaceChangesDiff(QString());
    m_patch = new DiffView(QString(), this);
    m_patch->setEmptyMessage(i18n("Loading patch…"));
    m_patchSlot->addWidget(m_patch);

    // Remembered size (persisted in KConfig like ToolInspectorDialog).
    const KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("CommitDetailDialog"));
    resize(cfg.readEntry("size", QSize(880, 680)));

    m_header->setText(QStringLiteral("<i>%1</i>")
                          .arg(i18n("Loading commit %1…", sha.left(8))
                                   .toHtmlEscaped()));
    loadDetail();
    loadPatch();
}

CommitDetailDialog::~CommitDetailDialog()
{
    KConfigGroup cfg =
        KSharedConfig::openConfig()->group(QStringLiteral("CommitDetailDialog"));
    cfg.writeEntry("size", size());
}

QJsonObject CommitDetailDialog::sourceParams() const
{
    QJsonObject p{{QStringLiteral("sha"), m_sha}};
    if (!m_threadId.isEmpty()) {
        p.insert(QStringLiteral("threadId"), m_threadId);
    } else if (!m_repoRoot.isEmpty()) {
        p.insert(QStringLiteral("repoRoot"), m_repoRoot);
    }
    return p;
}

void CommitDetailDialog::loadDetail()
{
    if (!m_core || !m_core->isConnected() || m_sha.isEmpty()) {
        return;
    }
    QPointer<CommitDetailDialog> guard(this);
    m_core->call(QStringLiteral("git.commit.detail"), sourceParams(),
                 [this, guard](const QJsonObject &result, const QJsonObject &error) {
                     if (!guard) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         m_header->setText(
                             QStringLiteral("<b>%1</b> %2")
                                 .arg(i18n("Error:").toHtmlEscaped(),
                                      error.value(QStringLiteral("message"))
                                          .toString()
                                          .toHtmlEscaped()));
                         return;
                     }
                     applyDetail(result);
                 },
                 this); // lifetime guard against late reply after dialog destruction
}

void CommitDetailDialog::loadPatch()
{
    if (!m_core || !m_core->isConnected() || m_sha.isEmpty()) {
        return;
    }
    QPointer<CommitDetailDialog> guard(this);
    QJsonObject params = sourceParams();
    params.remove(QStringLiteral("path")); // whole-commit patch
    m_core->call(QStringLiteral("git.commit.diff"), params,
                 [this, guard](const QJsonObject &result, const QJsonObject &error) {
                     if (!guard) {
                         return;
                     }
                     QString patch;
                     if (error.isEmpty()) {
                         patch = result.value(QStringLiteral("patch")).toString();
                     }
                     if (m_patch) {
                         m_patch->deleteLater();
                     }
                     m_patch = new DiffView(patch, this);
                     if (patch.isEmpty()) {
                         m_patch->setEmptyMessage(i18n("No changes in this commit."));
                     }
                     m_patchSlot->addWidget(m_patch);
                 },
                 this);
}

void CommitDetailDialog::loadFileDiff(const QString &path)
{
    if (!m_core || !m_core->isConnected() || m_sha.isEmpty()) {
        return;
    }
    QPointer<CommitDetailDialog> guard(this);
    const quint64 req = ++m_fileDiffReq;
    QJsonObject params = sourceParams();
    if (!path.isEmpty()) {
        params.insert(QStringLiteral("path"), path);
    }
    m_core->call(QStringLiteral("git.commit.diff"), params,
                 [this, guard, req](const QJsonObject &result, const QJsonObject &error) {
                     // Discard a reply a newer file selection has superseded, so
                     // out-of-order replies can't leave a stale diff on screen.
                     if (!guard || req != m_fileDiffReq) {
                         return;
                     }
                     QString patch;
                     if (error.isEmpty()) {
                         patch = result.value(QStringLiteral("patch")).toString();
                     }
                     replaceChangesDiff(patch);
                 },
                 this);
}

void CommitDetailDialog::applyDetail(const QJsonObject &detail)
{
    const QString sha = detail.value(QStringLiteral("sha")).toString();
    const QString shortSha = detail.value(QStringLiteral("shortSha")).toString();
    const QString subject = detail.value(QStringLiteral("subject")).toString();
    const QString author = detail.value(QStringLiteral("author")).toString();
    const QString authorEmail = detail.value(QStringLiteral("authorEmail")).toString();
    const QDateTime when = QDateTime::fromString(
        detail.value(QStringLiteral("authorTime")).toString(), Qt::ISODate);

    const QPalette pal = palette();
    QString html;
    html += QStringLiteral("<div style='font-size:120%;'><b>%1</b></div>")
                .arg(subject.toHtmlEscaped());

    // Chip line: short sha, author-initials chip, ref chips.
    QStringList chips;
    if (!shortSha.isEmpty()) {
        chips << QStringLiteral("<tt>%1</tt>").arg(shortSha.toHtmlEscaped());
    }
    if (!author.isEmpty()) {
        const AkColors &ak = ThemeManager::palette();
        chips << htmlChip(initials(author), ak.accent, ak.accentText)
                     + QStringLiteral(" %1").arg(author.toHtmlEscaped());
    }
    const QJsonArray refs = detail.value(QStringLiteral("refs")).toArray();
    for (const QJsonValue &v : refs) {
        chips << refChipHtml(v.toString(), pal);
    }
    if (!chips.isEmpty()) {
        html += QStringLiteral("<div style='margin-top:4px;'>%1</div>")
                    .arg(chips.join(QStringLiteral(" &nbsp; ")));
    }

    // Date line: absolute + relative, tooltip carries the ISO form.
    if (when.isValid()) {
        const QDateTime local = when.toLocalTime();
        const QString absolute = local.toString(Qt::ISODate);
        const QString relative =
            KFormat().formatRelativeDateTime(local, QLocale::LongFormat);
        html += QStringLiteral("<div style='opacity:0.75;margin-top:4px;'>%1 · %2</div>")
                    .arg(absolute.toHtmlEscaped(), relative.toHtmlEscaped());
    }
    m_header->setText(html);
    m_header->setToolTip(sha);

    m_body->setText(detail.value(QStringLiteral("body")).toString());
    m_body->setVisible(!m_body->text().isEmpty());

    m_files->clear();
    {
        auto *all = new QListWidgetItem(
            i18nc("synthetic entry that shows the whole commit diff", "All files"),
            m_files);
        QFont f = all->font();
        f.setItalic(true);
        all->setFont(f);
        all->setData(Qt::UserRole, QString()); // empty path = no filter
    }
    const QJsonArray files = detail.value(QStringLiteral("files")).toArray();
    for (const QJsonValue &v : files) {
        const QJsonObject f = v.toObject();
        const QString path = f.value(QStringLiteral("path")).toString();
        const QString oldPath = f.value(QStringLiteral("oldPath")).toString();
        const QString status = f.value(QStringLiteral("status")).toString();
        const int added = f.value(QStringLiteral("added")).toInt();
        const int deleted = f.value(QStringLiteral("deleted")).toInt();
        const QString label =
            oldPath.isEmpty() || oldPath == path
                ? QStringLiteral("%1  %2  %3")
                      .arg(statusGlyph(status), path, numstat(added, deleted))
                : QStringLiteral("%1  %2 → %3  %4")
                      .arg(statusGlyph(status), oldPath, path,
                           numstat(added, deleted));
        auto *item = new QListWidgetItem(label, m_files);
        item->setData(Qt::UserRole, path);
        item->setForeground(statusColor(status));
        item->setToolTip(path);
    }
    if (m_files->count() > 0) {
        m_files->setCurrentRow(0);
    }
}

void CommitDetailDialog::replaceChangesDiff(const QString &patch)
{
    if (m_changesDiff) {
        m_changesDiff->deleteLater();
        m_changesDiff = nullptr;
    }
    m_changesDiff = new DiffView(patch, this);
    if (patch.isEmpty()) {
        m_changesDiff->setEmptyMessage(i18n("No changes."));
    }
    m_changesDiffSlot->addWidget(m_changesDiff);
}

void CommitDetailDialog::onFileRowChanged(int row)
{
    if (row < 0) {
        return;
    }
    QListWidgetItem *item = m_files->item(row);
    if (!item) {
        return;
    }
    loadFileDiff(item->data(Qt::UserRole).toString());
}
