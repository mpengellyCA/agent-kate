#pragma once

#include <QAbstractTableModel>
#include <QString>
#include <QStringList>
#include <QVector>
#include <QWidget>

class QTableView;

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

// CsvView renders a CSV/TSV file as a sortable, selectable table inside an
// editor tab — far more useful than showing the raw delimited text. Read-only.
// A QSortFilterProxyModel gives click-to-sort columns; the header row is frozen
// as the table header.
class CsvView : public QWidget
{
    Q_OBJECT
public:
    explicit CsvView(const QString &path, QWidget *parent = nullptr);

    QString path() const { return m_path; }

    // True for .csv / .tsv files (by suffix), so EditorArea routes them here.
    static bool canDisplay(const QString &path);

private:
    QString m_path;
    QTableView *m_table = nullptr;
};
