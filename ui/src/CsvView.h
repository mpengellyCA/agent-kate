#pragma once

#include "XlsxReader.h"

#include <QAbstractTableModel>
#include <QString>
#include <QStringList>
#include <QVector>
#include <QWidget>

class QFileSystemWatcher;
class QTableView;
class QTimer;

// Read-only table model backing CsvView. The file's first record is treated as
// the column header; the parsed grid is held in memory (fine for typical CSVs).
// Honours RFC-4180 quoting (doubled quotes, delimiters/newlines inside quotes).
class CsvModel : public QAbstractTableModel
{
    Q_OBJECT
public:
    explicit CsvModel(QObject *parent = nullptr);

    // Parses `path`, auto-detecting ',' vs '\t'. Returns false if unreadable.
    bool load(const QString &path);

    // Populates the model from an already-parsed grid (e.g. one .xlsx sheet),
    // treating the first record as the header — same shape `load()` produces.
    void setRecords(QVector<QStringList> records);

    int rowCount(const QModelIndex &parent = QModelIndex()) const override;
    int columnCount(const QModelIndex &parent = QModelIndex()) const override;
    QVariant data(const QModelIndex &index, int role = Qt::DisplayRole) const override;
    QVariant headerData(int section, Qt::Orientation orientation,
                        int role = Qt::DisplayRole) const override;

private:
    QStringList m_header;
    QVector<QStringList> m_rows;
    int m_columns = 0;
};

// CsvView renders a CSV/TSV file — or an .xlsx/.xlsm workbook — as a sortable,
// selectable table inside an editor tab, far more useful than the raw delimited
// text or (for xlsx) the zip internals a generic archive part would show.
// Read-only. A QSortFilterProxyModel gives click-to-sort columns; the header row
// is frozen as the table header. Multi-sheet workbooks get a sheet selector.
class CsvView : public QWidget
{
    Q_OBJECT
public:
    explicit CsvView(const QString &path, QWidget *parent = nullptr);

    QString path() const { return m_path; }

    // True for .csv/.tsv and .xlsx/.xlsm files (by suffix), so EditorArea routes
    // them here ahead of the generic KPart viewer (which would open .xlsx as a
    // zip archive in Ark).
    static bool canDisplay(const QString &path);

private:
    // (Re)load the file into the model and resize columns. Used on construction
    // and when the on-disk file changes (an agent rewrote it).
    void reload();

    QString m_path;
    QTableView *m_table = nullptr;
    CsvModel *m_model = nullptr;
    QVector<XlsxSheet> m_sheets; // workbook sheets, kept for the sheet selector
    int m_currentSheet = 0;      // selected workbook sheet, preserved across reloads
    QFileSystemWatcher *m_watcher = nullptr;
    QTimer *m_reloadDebounce = nullptr;
};
