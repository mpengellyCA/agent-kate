// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "CommitDetailPanel.h"
#include "DiffView.h"
#include "ipc/CoreClient.h"

#include <QDateTime>
#include <QFont>
#include <QJsonArray>
#include <QJsonValue>
#include <QLabel>
#include <QListWidget>
#include <QListWidgetItem>
#include <QPlainTextEdit>
#include <QSplitter>
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

QString numstat(int added, int deleted)
{
    if (added < 0 || deleted < 0) {
        return QStringLiteral("bin");
    }
    return QStringLiteral("+%1 −%2").arg(added).arg(deleted);
}
} // namespace

CommitDetailPanel::CommitDetailPanel(CoreClient *core, QWidget *parent)
    : QWidget(parent)
    , m_core(core)
{
    m_header = new QLabel(this);
    m_header->setTextFormat(Qt::RichText);
    m_header->setTextInteractionFlags(Qt::TextSelectableByMouse);
    m_header->setWordWrap(true);

    m_body = new QPlainTextEdit(this);
    m_body->setReadOnly(true);
    m_body->setFrameShape(QFrame::NoFrame);
    QFont mono = m_body->font();
    mono.setFamily(QStringLiteral("monospace"));
    mono.setStyleHint(QFont::TypeWriter);
    m_body->setFont(mono);
    m_body->setMaximumHeight(140);

    m_files = new QListWidget(this);
    m_files->setAlternatingRowColors(true);
    m_files->setSelectionMode(QAbstractItemView::SingleSelection);
    connect(m_files, &QListWidget::currentRowChanged, this,
            &CommitDetailPanel::onFileRowChanged);

    auto *diffHost = new QWidget(this);
    m_diffSlot = new QVBoxLayout(diffHost);
    m_diffSlot->setContentsMargins(0, 0, 0, 0);

    auto *split = new QSplitter(Qt::Vertical, this);
    auto *topHost = new QWidget(split);
    auto *topLayout = new QVBoxLayout(topHost);
    topLayout->setContentsMargins(0, 0, 0, 0);
    topLayout->setSpacing(6);
    topLayout->addWidget(m_header);
    topLayout->addWidget(m_body);
    topLayout->addWidget(m_files, 1);
    split->addWidget(topHost);
    split->addWidget(diffHost);
    split->setStretchFactor(0, 1);
    split->setStretchFactor(1, 2);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(8, 8, 8, 8);
    layout->setSpacing(6);
    layout->addWidget(split);

    clear();
}

void CommitDetailPanel::clear()
{
    m_threadId.clear();
    m_repoRoot.clear();
    m_sha.clear();
    ++m_token;
    m_header->setText(QStringLiteral("<i>Select a commit to see details.</i>"));
    m_body->clear();
    m_files->clear();
    replaceDiff(QString());
}

bool CommitDetailPanel::hasSource() const
{
    return !m_threadId.isEmpty() || !m_repoRoot.isEmpty();
}

QJsonObject CommitDetailPanel::sourceParams() const
{
    QJsonObject p{{QStringLiteral("sha"), m_sha}};
    if (!m_threadId.isEmpty()) {
        p.insert(QStringLiteral("threadId"), m_threadId);
    } else if (!m_repoRoot.isEmpty()) {
        p.insert(QStringLiteral("repoRoot"), m_repoRoot);
    }
    return p;
}

void CommitDetailPanel::setCommit(const QString &threadId, const QString &repoRoot, const QString &sha)
{
    if (threadId == m_threadId && repoRoot == m_repoRoot && sha == m_sha) {
        return;
    }
    m_threadId = threadId;
    m_repoRoot = repoRoot;
    m_sha = sha;
    ++m_token;
    m_header->setText(QStringLiteral("<i>Loading %1…</i>")
                          .arg(sha.left(8).toHtmlEscaped()));
    m_body->clear();
    m_files->clear();
    replaceDiff(QString());
    loadDetail();
    // Full-patch diff in parallel so the diff pane fills as fast as possible;
    // selecting a file in the list narrows it later.
    loadDiff(QString());
}

void CommitDetailPanel::loadDetail()
{
    if (!m_core->isConnected() || !hasSource() || m_sha.isEmpty()) {
        return;
    }
    const int token = m_token;
    m_core->call(QStringLiteral("git.commit.detail"),
                 sourceParams(),
                 [this, token](const QJsonObject &result, const QJsonObject &error) {
                     if (token != m_token) {
                         return;
                     }
                     if (!error.isEmpty()) {
                         m_header->setText(
                             QStringLiteral("<b>Error:</b> %1")
                                 .arg(error.value(QStringLiteral("message"))
                                          .toString()
                                          .toHtmlEscaped()));
                         return;
                     }
                     applyDetail(result);
                 });
}

void CommitDetailPanel::loadDiff(const QString &path)
{
    if (!m_core->isConnected() || !hasSource() || m_sha.isEmpty()) {
        return;
    }
    const int token = m_token;
    QJsonObject params = sourceParams();
    if (!path.isEmpty()) {
        params.insert(QStringLiteral("path"), path);
    }
    m_core->call(QStringLiteral("git.commit.diff"), params,
                 [this, token](const QJsonObject &result, const QJsonObject &error) {
                     if (token != m_token) {
                         return;
                     }
                     QString patch;
                     if (error.isEmpty()) {
                         patch = result.value(QStringLiteral("patch")).toString();
                     }
                     replaceDiff(patch);
                 });
}

void CommitDetailPanel::applyDetail(const QJsonObject &detail)
{
    const QString sha = detail.value(QStringLiteral("sha")).toString();
    const QString shortSha = detail.value(QStringLiteral("shortSha")).toString();
    const QString subject = detail.value(QStringLiteral("subject")).toString();
    const QString author = detail.value(QStringLiteral("author")).toString();
    const QString authorEmail = detail.value(QStringLiteral("authorEmail")).toString();
    const QDateTime when = QDateTime::fromString(
        detail.value(QStringLiteral("authorTime")).toString(), Qt::ISODate);

    QString headerHtml;
    headerHtml += QStringLiteral("<div style='font-size: 110%;'><b>%1</b></div>")
                      .arg(subject.toHtmlEscaped());
    QStringList meta;
    if (!shortSha.isEmpty()) {
        meta << QStringLiteral("<tt>%1</tt>").arg(shortSha.toHtmlEscaped());
    }
    if (!author.isEmpty()) {
        const QString who = authorEmail.isEmpty()
                                ? author
                                : QStringLiteral("%1 &lt;%2&gt;").arg(
                                      author.toHtmlEscaped(),
                                      authorEmail.toHtmlEscaped());
        meta << who;
    }
    if (when.isValid()) {
        meta << when.toLocalTime().toString(Qt::ISODate);
    }
    headerHtml += QStringLiteral("<div style='opacity:0.75;'>%1</div>")
                      .arg(meta.join(QStringLiteral("  •  ")));
    m_header->setText(headerHtml);
    m_header->setToolTip(sha);

    m_body->setPlainText(detail.value(QStringLiteral("body")).toString());

    m_files->clear();
    // Insert a synthetic "All files" entry that drops the path filter so the
    // user can pop back to the full patch after looking at one file.
    {
        auto *all = new QListWidgetItem(QStringLiteral("All files"), m_files);
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
        item->setToolTip(path);
    }
    if (m_files->count() > 0) {
        m_files->setCurrentRow(0);
    }
}

void CommitDetailPanel::replaceDiff(const QString &patch)
{
    if (m_diff) {
        m_diff->deleteLater();
        m_diff = nullptr;
    }
    const QString shown = patch.isEmpty() ? QStringLiteral("(no diff)") : patch;
    m_diff = new DiffView(shown, this);
    m_diffSlot->addWidget(m_diff);
}

void CommitDetailPanel::onFileRowChanged(int row)
{
    if (row < 0) {
        return;
    }
    QListWidgetItem *item = m_files->item(row);
    if (!item) {
        return;
    }
    loadDiff(item->data(Qt::UserRole).toString());
}
