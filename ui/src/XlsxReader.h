#pragma once

#include <QString>
#include <QStringList>
#include <QVector>

// One worksheet of a parsed .xlsx workbook: a display name plus the cell grid as
// rows of strings (ragged rows allowed — short rows are padded by the view). The
// first row is treated as the header by CsvView, matching its CSV handling.
struct XlsxSheet {
    QString name;
    QVector<QStringList> rows;
};

// Minimal, dependency-light reader for the OOXML spreadsheet (.xlsx/.xlsm)
// format. An .xlsx is a zip of XML parts; we read it with KZip and pull the
// shared-string table, workbook sheet list and per-sheet cell data with
// QXmlStreamReader. Values are surfaced as text — numbers verbatim, shared and
// inline strings resolved; style-driven date/number formatting is not applied.
namespace XlsxReader {

// Parses `path` into its worksheets in workbook order. Returns false (and leaves
// `sheets` untouched) if the file can't be opened as a zip or has no readable
// worksheet; an empty-but-valid workbook yields true with zero sheets.
bool read(const QString &path, QVector<XlsxSheet> &sheets);

} // namespace XlsxReader
