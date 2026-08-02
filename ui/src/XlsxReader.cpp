#include "XlsxReader.h"

#include <KArchiveDirectory>
#include <KArchiveFile>
#include <KZip>

#include <QHash>
#include <QXmlStreamReader>

namespace {

// The relationships namespace that carries a sheet's r:id on <sheet> elements.
const QString kRelNs =
    QStringLiteral("http://schemas.openxmlformats.org/officeDocument/2006/relationships");

// Bounded-read budgets (audit F55/F56). OOXML's own column maximum is 16384
// ("XFD") — anything past it is a crafted ref, not a spreadsheet. The part cap
// gates a zip entry's DECLARED uncompressed size before data() materialises it
// (the zip-bomb shape: a gigabyte sharedStrings.xml in a kilobyte file), and
// the row/cell caps bound what a sheet may expand to in memory. Generous for
// real workbooks, fatal for bombs.
constexpr int kMaxColumns = 16384;
constexpr qint64 kMaxPartBytes = 32 * 1024 * 1024;
constexpr int kMaxRowsPerSheet = 100000;
constexpr qsizetype kMaxCellsPerSheet = 1000000;
// The per-sheet budgets multiply by the sheet count, and workbook.xml can name
// one tiny worksheet part a thousand times (duplicate r:id targets are legal
// zip-wise) — so the workbook loop needs its own caps or a ~50 KB manifest
// re-opens the OOM the per-sheet caps closed. Aggregate cells is the binding
// budget; the sheet-count cap just keeps the picker sane.
constexpr int kMaxSheets = 64;
constexpr qsizetype kMaxCellsPerWorkbook = 4 * kMaxCellsPerSheet;

// Read the raw bytes of a zip entry by its full path (e.g. "xl/workbook.xml").
// Returns an empty array when the entry is missing, is a directory, or claims
// an uncompressed size past kMaxPartBytes — the check runs BEFORE data() so an
// oversized part is never decompressed at all; `refused` reports that case.
QByteArray entryData(const KArchiveDirectory *root, const QString &path, bool *refused = nullptr)
{
    const KArchiveEntry *e = root->entry(path);
    if (!e || !e->isFile()) {
        return {};
    }
    const auto *file = static_cast<const KArchiveFile *>(e);
    if (file->size() > kMaxPartBytes) {
        if (refused) {
            *refused = true;
        }
        return {};
    }
    return file->data();
}

// Convert a cell column reference ("A", "B", ... "AA") to a 0-based index. The
// trailing row digits in a full ref like "AB12" are ignored. Accumulation stops
// the moment the running value passes kMaxColumns, so a crafted "ZZZZZZZZ1" can
// neither overflow the int (UB) nor name a column to pad out to; out-of-range
// refs report -1 and the caller falls back to sequential placement.
int columnIndex(QStringView ref)
{
    int idx = 0;
    for (const QChar c : ref) {
        if (c < QLatin1Char('A') || c > QLatin1Char('Z')) {
            break;
        }
        idx = idx * 26 + (c.unicode() - 'A' + 1);
        if (idx > kMaxColumns) {
            return -1;
        }
    }
    return idx - 1; // map A→0; a refless cell yields -1
}

// Parse xl/sharedStrings.xml into an indexed table. Each <si> may hold a single
// <t> or several <r><t> rich-text runs, which we concatenate.
QVector<QString> readSharedStrings(const QByteArray &xml)
{
    QVector<QString> strings;
    QXmlStreamReader r(xml);
    QString current;
    bool inItem = false;
    while (!r.atEnd()) {
        const auto t = r.readNext();
        if (t == QXmlStreamReader::StartElement) {
            if (r.name() == QLatin1String("si")) {
                inItem = true;
                current.clear();
            } else if (inItem && r.name() == QLatin1String("t")) {
                current += r.readElementText();
            }
        } else if (t == QXmlStreamReader::EndElement && r.name() == QLatin1String("si")) {
            strings.append(current);
            inItem = false;
        }
    }
    return strings;
}

// Parse a worksheet part into a row grid, resolving shared-string and inline
// values. Sparse rows/cells are honoured via the r="A1" references so columns
// line up; gaps are left as empty fields. Materialisation is bounded (audit
// F55): parsing stops at kMaxRowsPerSheet rows or kMaxCellsPerSheet total
// cells, reporting the cut via `truncated`.
QVector<QStringList> readSheet(const QByteArray &xml, const QVector<QString> &shared,
                               bool *truncated)
{
    QVector<QStringList> rows;
    QXmlStreamReader r(xml);

    QStringList row;
    int col = 0;          // running column for the cell about to be read
    QString cellType;     // t attribute: "s", "inlineStr", "str", "b", or empty
    qsizetype cells = 0;  // total cells materialised across appended rows

    while (!r.atEnd()) {
        const auto tok = r.readNext();
        if (tok == QXmlStreamReader::StartElement) {
            if (r.name() == QLatin1String("row")) {
                row.clear();
                col = 0;
            } else if (r.name() == QLatin1String("c")) {
                const auto attrs = r.attributes();
                cellType = attrs.value(QLatin1String("t")).toString();
                const auto ref = attrs.value(QLatin1String("r"));
                if (!ref.isEmpty()) {
                    const int c = columnIndex(ref);
                    if (c >= 0) {
                        col = c; // jump to the cell's true column, padding the gap
                    }
                }
                while (row.size() < col) {
                    row.append(QString());
                }
            } else if (r.name() == QLatin1String("v")) {
                const QString raw = r.readElementText();
                if (col >= kMaxColumns) {
                    continue; // past OOXML's column maximum — drop, don't grow
                }
                QString value = raw;
                if (cellType == QLatin1String("s")) {
                    const int i = raw.toInt();
                    value = (i >= 0 && i < shared.size()) ? shared.at(i) : QString();
                } else if (cellType == QLatin1String("b")) {
                    value = raw == QLatin1String("1") ? QStringLiteral("TRUE")
                                                      : QStringLiteral("FALSE");
                }
                if (row.size() <= col) {
                    row.append(value);
                } else {
                    row[col] = value;
                }
                ++col;
            } else if (r.name() == QLatin1String("is")) {
                // Inline string: <c t="inlineStr"><is><t>…</t></is></c>.
                QString value;
                while (!r.atEnd()) {
                    const auto it = r.readNext();
                    if (it == QXmlStreamReader::StartElement && r.name() == QLatin1String("t")) {
                        value += r.readElementText();
                    } else if (it == QXmlStreamReader::EndElement
                               && r.name() == QLatin1String("is")) {
                        break;
                    }
                }
                if (col >= kMaxColumns) {
                    continue; // past OOXML's column maximum — drop, don't grow
                }
                if (row.size() <= col) {
                    row.append(value);
                } else {
                    row[col] = value;
                }
                ++col;
            }
        } else if (tok == QXmlStreamReader::EndElement && r.name() == QLatin1String("row")) {
            if (rows.size() >= kMaxRowsPerSheet || cells + row.size() > kMaxCellsPerSheet) {
                if (truncated) {
                    *truncated = true;
                }
                break;
            }
            cells += row.size();
            rows.append(row);
        }
    }
    return rows;
}

} // namespace

namespace XlsxReader {

bool read(const QString &path, QVector<XlsxSheet> &sheets)
{
    KZip zip(path);
    if (!zip.open(QIODevice::ReadOnly)) {
        return false;
    }
    const KArchiveDirectory *root = zip.directory();
    if (!root) {
        return false;
    }

    // A refused (oversized) shared-string table degrades every sheet — the
    // "s"-typed cells all resolve to empty — so the refusal is surfaced on each
    // sheet's truncated flag rather than silently showing a hollowed-out grid.
    bool sharedRefused = false;
    const QVector<QString> shared = readSharedStrings(
        entryData(root, QStringLiteral("xl/sharedStrings.xml"), &sharedRefused));

    // rId → worksheet part path, from xl/_rels/workbook.xml.rels.
    QHash<QString, QString> relTargets;
    {
        QXmlStreamReader r(entryData(root, QStringLiteral("xl/_rels/workbook.xml.rels")));
        while (!r.atEnd()) {
            if (r.readNext() == QXmlStreamReader::StartElement
                && r.name() == QLatin1String("Relationship")) {
                const auto attrs = r.attributes();
                const QString id = attrs.value(QLatin1String("Id")).toString();
                QString target = attrs.value(QLatin1String("Target")).toString();
                if (id.isEmpty() || target.isEmpty()) {
                    continue;
                }
                // Targets are relative to xl/ ("worksheets/sheet1.xml") unless
                // given as an absolute zip path ("/xl/worksheets/sheet1.xml").
                target = target.startsWith(QLatin1Char('/')) ? target.mid(1)
                                                             : QStringLiteral("xl/") + target;
                relTargets.insert(id, target);
            }
        }
    }

    // Ordered (name, rId) sheet list from xl/workbook.xml.
    QVector<XlsxSheet> result;
    {
        qsizetype workbookCells = 0;
        QXmlStreamReader r(entryData(root, QStringLiteral("xl/workbook.xml")));
        while (!r.atEnd()) {
            if (r.readNext() == QXmlStreamReader::StartElement
                && r.name() == QLatin1String("sheet")) {
                if (result.size() >= kMaxSheets || workbookCells >= kMaxCellsPerWorkbook) {
                    // Budget spent before this part is ever decompressed; flag
                    // the last visible sheet so the drop is not silent.
                    if (!result.isEmpty()) {
                        result.last().truncated = true;
                    }
                    break;
                }
                const auto attrs = r.attributes();
                const QString name = attrs.value(QLatin1String("name")).toString();
                const QString rid = attrs.value(kRelNs, QStringLiteral("id")).toString();
                const QString part = relTargets.value(rid);
                if (part.isEmpty()) {
                    continue;
                }
                XlsxSheet sheet;
                sheet.name = name;
                bool partRefused = false;
                bool rowsCut = false;
                sheet.rows = readSheet(entryData(root, part, &partRefused), shared, &rowsCut);
                sheet.truncated = partRefused || rowsCut || sharedRefused;
                for (const QVector<QString> &sheetRow : sheet.rows) {
                    workbookCells += sheetRow.size();
                }
                result.append(sheet);
            }
        }
    }

    if (result.isEmpty() && !root->entry(QStringLiteral("xl/workbook.xml"))) {
        return false; // not a spreadsheet package at all
    }
    sheets = result;
    return true;
}

} // namespace XlsxReader
