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

// Read the raw bytes of a zip entry by its full path (e.g. "xl/workbook.xml").
// Returns an empty array when the entry is missing or is a directory.
QByteArray entryData(const KArchiveDirectory *root, const QString &path)
{
    const KArchiveEntry *e = root->entry(path);
    if (!e || !e->isFile()) {
        return {};
    }
    return static_cast<const KArchiveFile *>(e)->data();
}

// Convert a cell column reference ("A", "B", ... "AA") to a 0-based index. The
// trailing row digits in a full ref like "AB12" are ignored.
int columnIndex(QStringView ref)
{
    int idx = 0;
    for (const QChar c : ref) {
        if (c < QLatin1Char('A') || c > QLatin1Char('Z')) {
            break;
        }
        idx = idx * 26 + (c.unicode() - 'A' + 1);
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
// line up; gaps are left as empty fields.
QVector<QStringList> readSheet(const QByteArray &xml, const QVector<QString> &shared)
{
    QVector<QStringList> rows;
    QXmlStreamReader r(xml);

    QStringList row;
    int col = 0;          // running column for the cell about to be read
    QString cellType;     // t attribute: "s", "inlineStr", "str", "b", or empty

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
                if (row.size() <= col) {
                    row.append(value);
                } else {
                    row[col] = value;
                }
                ++col;
            }
        } else if (tok == QXmlStreamReader::EndElement && r.name() == QLatin1String("row")) {
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

    const QVector<QString> shared = readSharedStrings(entryData(root, QStringLiteral("xl/sharedStrings.xml")));

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
        QXmlStreamReader r(entryData(root, QStringLiteral("xl/workbook.xml")));
        while (!r.atEnd()) {
            if (r.readNext() == QXmlStreamReader::StartElement
                && r.name() == QLatin1String("sheet")) {
                const auto attrs = r.attributes();
                const QString name = attrs.value(QLatin1String("name")).toString();
                const QString rid = attrs.value(kRelNs, QStringLiteral("id")).toString();
                const QString part = relTargets.value(rid);
                if (part.isEmpty()) {
                    continue;
                }
                XlsxSheet sheet;
                sheet.name = name;
                sheet.rows = readSheet(entryData(root, part), shared);
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
