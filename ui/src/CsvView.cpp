#include "CsvView.h"

#include <QComboBox>
#include <QFile>
#include <QFileInfo>
#include <QFileSystemWatcher>
#include <QHBoxLayout>
#include <QHeaderView>
#include <QLabel>
#include <QSortFilterProxyModel>
#include <QTableView>
#include <QTimer>
#include <QVBoxLayout>

#include <utility>

namespace {

// Bounded-read budgets (audit F55). Generous for real files, fatal for bombs:
// a delimited file past 32 MB or 100k records renders truncated with a visible
// note instead of freezing the GUI thread or ballooning the model.
constexpr qsizetype kMaxCsvBytes = 32 * 1024 * 1024;
constexpr int kMaxCsvRecords = 100000;

// Auto-detect the delimiter from the header line, counting only separators that
// sit outside quotes. Tab wins over comma only when tabs actually appear,
// otherwise we default to comma.
QChar detectDelimiter(const QString &firstLine)
{
    int commas = 0;
    int tabs = 0;
    bool inQuotes = false;
    for (const QChar c : firstLine) {
        if (c == QLatin1Char('"')) {
            inQuotes = !inQuotes;
        } else if (!inQuotes && c == QLatin1Char(',')) {
            ++commas;
        } else if (!inQuotes && c == QLatin1Char('\t')) {
            ++tabs;
        }
    }
    return tabs > commas ? QLatin1Char('\t') : QLatin1Char(',');
}

// RFC-4180-style parse of `text` into records of fields: doubled quotes become a
// literal quote, and delimiters/newlines inside quotes are kept verbatim. Both
// CRLF and bare LF/CR terminate a record. Parsing stops at kMaxCsvRecords
// records; `recordCapped` reports whether input remained past the cut.
QVector<QStringList> parseDelimited(const QString &text, QChar delim,
                                    bool *recordCapped = nullptr)
{
    QVector<QStringList> records;
    QStringList fields;
    QString cur;
    bool inQuotes = false;

    auto endField = [&] {
        fields.append(cur);
        cur.clear();
    };
    auto endRecord = [&] {
        endField();
        records.append(fields);
        fields.clear();
    };

    const int n = text.size();
    int i = 0;
    for (; i < n; ++i) {
        if (records.size() >= kMaxCsvRecords) {
            break;
        }
        const QChar c = text.at(i);
        if (inQuotes) {
            if (c == QLatin1Char('"')) {
                if (i + 1 < n && text.at(i + 1) == QLatin1Char('"')) {
                    cur.append(QLatin1Char('"'));
                    ++i;
                } else {
                    inQuotes = false;
                }
            } else {
                cur.append(c);
            }
        } else if (c == QLatin1Char('"')) {
            inQuotes = true;
        } else if (c == delim) {
            endField();
        } else if (c == QLatin1Char('\n')) {
            endRecord();
        } else if (c == QLatin1Char('\r')) {
            if (i + 1 < n && text.at(i + 1) == QLatin1Char('\n')) {
                ++i; // swallow the LF of a CRLF pair
            }
            endRecord();
        } else {
            cur.append(c);
        }
    }
    if (records.size() >= kMaxCsvRecords) {
        // Capped mid-file: anything left (unscanned text or a half-built
        // record) is dropped, and only then does the cap count as truncation.
        if (recordCapped && (i < n || !cur.isEmpty() || !fields.isEmpty())) {
            *recordCapped = true;
        }
    } else if (!cur.isEmpty() || !fields.isEmpty()) {
        // Flush a final record that wasn't newline-terminated.
        endRecord();
    }
    return records;
}

// QSortFilterProxyModel sorts lexicographically by default, so "10" lands before
// "2". Compare numerically when both cells parse as numbers, else fall back to a
// case-insensitive string compare.
class CsvSortProxy : public QSortFilterProxyModel
{
    Q_OBJECT
public:
    using QSortFilterProxyModel::QSortFilterProxyModel;

protected:
    bool lessThan(const QModelIndex &left, const QModelIndex &right) const override
    {
        const QString l = sourceModel()->data(left, Qt::DisplayRole).toString();
        const QString r = sourceModel()->data(right, Qt::DisplayRole).toString();
        bool lok = false;
        bool rok = false;
        const double ln = l.toDouble(&lok);
        const double rn = r.toDouble(&rok);
        if (lok && rok) {
            return ln < rn;
        }
        return QString::compare(l, r, Qt::CaseInsensitive) < 0;
    }
};

} // namespace

CsvModel::CsvModel(QObject *parent)
    : QAbstractTableModel(parent)
{
}

bool CsvModel::load(const QString &path)
{
    QFile file(path);
    if (!file.open(QIODevice::ReadOnly)) {
        return false;
    }
    // Bound the read BEFORE parsing (audit F55), one byte past the cap so
    // "exactly the cap" and "the cap, and there was more" are distinguishable —
    // AttachmentBuilder's readCapped idiom. The capped read is the bound itself,
    // never readAll(): a stat'd size can lie (procfs, a file still growing).
    QByteArray raw = file.read(kMaxCsvBytes + 1);
    m_byteTruncated = raw.size() > kMaxCsvBytes;
    if (m_byteTruncated) {
        raw.truncate(kMaxCsvBytes);
    }
    QString text = QString::fromUtf8(raw);
    if (text.startsWith(QChar(0xFEFF))) {
        text.remove(0, 1); // strip a UTF-8 BOM so it can't pollute the header
    }

    const int firstBreak = text.indexOf(QLatin1Char('\n'));
    const QString firstLine = firstBreak < 0 ? text : text.left(firstBreak);
    const QChar delim = detectDelimiter(firstLine);

    bool recordCapped = false;
    setRecords(parseDelimited(text, delim, &recordCapped));
    m_recordTruncated = recordCapped;
    return true;
}

void CsvModel::setRecords(QVector<QStringList> records)
{
    beginResetModel();
    m_header.clear();
    m_rows.clear();
    m_columns = 0;
    if (!records.isEmpty()) {
        m_header = records.takeFirst();
        m_rows = std::move(records);
        m_columns = m_header.size();
        for (const QStringList &row : std::as_const(m_rows)) {
            m_columns = qMax(m_columns, row.size());
        }
    }
    endResetModel();
}

int CsvModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_rows.size();
}

int CsvModel::columnCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : m_columns;
}

QVariant CsvModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || (role != Qt::DisplayRole && role != Qt::ToolTipRole)) {
        return {};
    }
    const QStringList &row = m_rows.at(index.row());
    if (index.column() >= row.size()) {
        return {}; // ragged row — this record has fewer fields than the widest
    }
    return row.at(index.column());
}

QVariant CsvModel::headerData(int section, Qt::Orientation orientation, int role) const
{
    if (role != Qt::DisplayRole) {
        return {};
    }
    if (orientation == Qt::Vertical) {
        return section + 1; // 1-based data-row number
    }
    if (section < m_header.size() && !m_header.at(section).isEmpty()) {
        return m_header.at(section);
    }
    return QStringLiteral("Column %1").arg(section + 1);
}

bool CsvView::canDisplay(const QString &path)
{
    const QString suffix = QFileInfo(path).suffix().toLower();
    return suffix == QLatin1String("csv") || suffix == QLatin1String("tsv")
        || suffix == QLatin1String("xlsx") || suffix == QLatin1String("xlsm");
}

CsvView::CsvView(const QString &path, QWidget *parent)
    : QWidget(parent)
    , m_path(QFileInfo(path).absoluteFilePath())
{
    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);

    m_model = new CsvModel(this);
    const QString suffix = QFileInfo(m_path).suffix().toLower();
    const bool isWorkbook =
        suffix == QLatin1String("xlsx") || suffix == QLatin1String("xlsm");

    bool ok = false;
    if (isWorkbook) {
        ok = XlsxReader::read(m_path, m_sheets) && !m_sheets.isEmpty();
        if (ok) {
            // A multi-sheet workbook gets a selector strip above the table;
            // switching repopulates the same model with the chosen sheet.
            if (m_sheets.size() > 1) {
                auto *bar = new QHBoxLayout;
                bar->setContentsMargins(4, 4, 4, 0);
                bar->addWidget(new QLabel(tr("Sheet:"), this));
                auto *picker = new QComboBox(this);
                for (const XlsxSheet &sheet : std::as_const(m_sheets)) {
                    picker->addItem(sheet.name);
                }
                connect(picker, &QComboBox::currentIndexChanged, this, [this](int i) {
                    if (i >= 0 && i < m_sheets.size()) {
                        m_currentSheet = i;
                        m_model->setRecords(m_sheets.at(i).rows);
                        m_table->resizeColumnsToContents();
                        updateTruncationNote();
                    }
                });
                bar->addWidget(picker);
                bar->addStretch();
                layout->addLayout(bar);
            }
            m_model->setRecords(m_sheets.first().rows);
        }
    } else {
        ok = m_model->load(m_path);
    }

    if (!ok) {
        auto *msg = new QLabel(tr("Could not read %1").arg(QFileInfo(m_path).fileName()), this);
        msg->setAlignment(Qt::AlignCenter);
        msg->setStyleSheet(QStringLiteral("color: palette(mid);"));
        layout->addWidget(msg);
        return;
    }

    // Truncation is a data-integrity fact the user must see (audit F55): when a
    // size budget cut the grid short, say so instead of silently showing less.
    m_truncNote = new QLabel(this);
    m_truncNote->setContentsMargins(6, 4, 6, 4);
    m_truncNote->setStyleSheet(QStringLiteral("color: palette(mid);"));
    m_truncNote->setVisible(false);
    layout->addWidget(m_truncNote);

    auto *proxy = new CsvSortProxy(this);
    proxy->setSourceModel(m_model);

    m_table = new QTableView(this);
    m_table->setModel(proxy);
    m_table->setSortingEnabled(true);
    // setSortingEnabled() eagerly sorts by column 0; reset to an empty indicator
    // so the file's natural row order is shown on load. Clicking a header still
    // sorts (ascending first), via the numeric-aware proxy above.
    m_table->horizontalHeader()->setSortIndicator(-1, Qt::AscendingOrder);
    m_table->setAlternatingRowColors(true);
    m_table->setSelectionMode(QAbstractItemView::ContiguousSelection);
    m_table->setSelectionBehavior(QAbstractItemView::SelectItems);
    m_table->setEditTriggers(QAbstractItemView::NoEditTriggers);
    m_table->setWordWrap(false);
    m_table->horizontalHeader()->setStretchLastSection(true);
    // Bound the cost of content-based sizing on large files: sample a fixed
    // number of rows per column rather than scanning the whole table.
    m_table->horizontalHeader()->setResizeContentsPrecision(64);
    m_table->resizeColumnsToContents();

    layout->addWidget(m_table);
    updateTruncationNote();

    // Re-read when an agent rewrites the file on disk. A short debounce
    // coalesces the burst of events an editor's save can emit, and the path is
    // re-added each time because many editors save atomically (rename-into-
    // place), which drops the original inode the watcher was tracking.
    m_reloadDebounce = new QTimer(this);
    m_reloadDebounce->setSingleShot(true);
    m_reloadDebounce->setInterval(150);
    connect(m_reloadDebounce, &QTimer::timeout, this, &CsvView::reload);

    m_watcher = new QFileSystemWatcher(this);
    m_watcher->addPath(m_path);
    connect(m_watcher, &QFileSystemWatcher::fileChanged, this, [this](const QString &) {
        m_reloadDebounce->start();
    });
}

void CsvView::reload()
{
    if (!m_model || !m_table) {
        return;
    }
    const QString suffix = QFileInfo(m_path).suffix().toLower();
    const bool isWorkbook =
        suffix == QLatin1String("xlsx") || suffix == QLatin1String("xlsm");
    if (isWorkbook) {
        QVector<XlsxSheet> sheets;
        if (XlsxReader::read(m_path, sheets) && !sheets.isEmpty()) {
            m_sheets = sheets;
            // Keep showing the same sheet the user had selected, if it still
            // exists after the rewrite (clamp otherwise).
            const int sheet = qBound(0, m_currentSheet, m_sheets.size() - 1);
            m_model->setRecords(m_sheets.at(sheet).rows);
            m_table->resizeColumnsToContents();
        }
    } else if (m_model->load(m_path)) {
        m_table->resizeColumnsToContents();
    }
    updateTruncationNote();
    // An atomic rewrite replaces the inode, so the watcher silently stops
    // tracking the file after the first change. Re-add the path if it dropped.
    if (m_watcher && !m_watcher->files().contains(m_path)
        && QFileInfo::exists(m_path)) {
        m_watcher->addPath(m_path);
    }
}

void CsvView::updateTruncationNote()
{
    if (!m_truncNote || !m_model) {
        return;
    }
    const QString suffix = QFileInfo(m_path).suffix().toLower();
    const bool isWorkbook =
        suffix == QLatin1String("xlsx") || suffix == QLatin1String("xlsm");
    QString note;
    if (isWorkbook) {
        if (m_currentSheet >= 0 && m_currentSheet < m_sheets.size()
            && m_sheets.at(m_currentSheet).truncated) {
            note = tr("Sheet truncated — too large to display fully");
        }
    } else if (m_model->byteTruncated()) {
        note = tr("Truncated at %1 MB — the file is larger")
                   .arg(kMaxCsvBytes / (1024 * 1024));
    } else if (m_model->recordTruncated()) {
        note = tr("Truncated at %1 rows — the file has more").arg(kMaxCsvRecords);
    }
    m_truncNote->setText(note);
    m_truncNote->setVisible(!note.isEmpty());
}

#include "CsvView.moc"
