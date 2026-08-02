// Audit F55/F56: the delimited-file viewers must stay bounded against hostile
// input an agent with file tools can write in one call. Pins four properties:
//
//   * a crafted cell ref ("ZZZZZZ1") cannot balloon a row past OOXML's own
//     16384-column maximum — and a longer ref cannot overflow the accumulator;
//   * an oversized zip part (the gigabyte-sharedStrings zip-bomb shape) is
//     refused from its DECLARED size, before it is ever decompressed;
//   * a sheet stops materialising at the row/cell budgets and says so;
//   * CsvModel truncates an oversize CSV at the byte and record caps, and
//     CsvView surfaces the truncation visibly.
//
// The .xlsx fixtures are real zips written with KZip carrying the real OOXML
// part paths and XML shapes — the same wire format the reader sees in anger.

#include "CsvView.h"
#include "XlsxReader.h"

#include <KZip>

#include <QFile>
#include <QLabel>
#include <QTemporaryDir>
#include <QtTest>

namespace {

// The budgets, pinned on purpose: if a cap in the viewer is raised, lowered or
// removed, a test here must notice.
constexpr int kMaxColumns = 16384;
constexpr qint64 kMaxPartBytes = 32 * 1024 * 1024;
constexpr int kMaxRowsPerSheet = 100000;
constexpr int kMaxSheets = 64;
constexpr qsizetype kMaxCsvBytes = 32 * 1024 * 1024;
constexpr int kMaxCsvRecords = 100000;

const QByteArray kContentTypes = QByteArrayLiteral(
    "<?xml version=\"1.0\"?>"
    "<Types xmlns=\"http://schemas.openxmlformats.org/package/2006/content-types\">"
    "<Default Extension=\"xml\" ContentType=\"application/xml\"/>"
    "<Override PartName=\"/xl/workbook.xml\" ContentType=\"application/vnd."
    "openxmlformats-officedocument.spreadsheetml.sheet.main+xml\"/>"
    "</Types>");

const QByteArray kWorkbook = QByteArrayLiteral(
    "<?xml version=\"1.0\"?>"
    "<workbook xmlns=\"http://schemas.openxmlformats.org/spreadsheetml/2006/main\""
    " xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\">"
    "<sheets><sheet name=\"Sheet1\" sheetId=\"1\" r:id=\"rId1\"/></sheets>"
    "</workbook>");

const QByteArray kWorkbookRels = QByteArrayLiteral(
    "<?xml version=\"1.0\"?>"
    "<Relationships xmlns=\"http://schemas.openxmlformats.org/package/2006/relationships\">"
    "<Relationship Id=\"rId1\" Type=\"http://schemas.openxmlformats.org/"
    "officeDocument/2006/relationships/worksheet\" Target=\"worksheets/sheet1.xml\"/>"
    "</Relationships>");

QByteArray wrapSheet(const QByteArray &sheetData)
{
    return QByteArrayLiteral(
               "<?xml version=\"1.0\"?>"
               "<worksheet xmlns=\"http://schemas.openxmlformats.org/"
               "spreadsheetml/2006/main\"><sheetData>")
        + sheetData + QByteArrayLiteral("</sheetData></worksheet>");
}

} // namespace

class CsvXlsxBoundsTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void readsMinimalWorkbook();
    void craftedRefStaysClamped();
    void refAtColumnLimitStillHonoured();
    void oversizedSharedStringsRefused();
    void rowCapTruncatesSheet();
    void cellCapTruncatesSheet();
    void sheetMultiplicationStaysBounded();
    void csvByteCapTruncates();
    void csvRecordCapTruncates();
    void smallCsvIsNotTruncated();
    void csvViewShowsTruncationNote();

private:
    // Build a real .xlsx zip with one worksheet (and optionally a shared-string
    // part) and return its path. Empty return means the write failed.
    QString writeXlsx(const QString &name, const QByteArray &sheetXml,
                      const QByteArray &sharedXml = {});

    QTemporaryDir m_dir;
};

void CsvXlsxBoundsTest::initTestCase()
{
    QVERIFY(m_dir.isValid());
}

QString CsvXlsxBoundsTest::writeXlsx(const QString &name, const QByteArray &sheetXml,
                                     const QByteArray &sharedXml)
{
    const QString path = m_dir.filePath(name);
    KZip zip(path);
    if (!zip.open(QIODevice::WriteOnly)) {
        return {};
    }
    bool ok = zip.writeFile(QStringLiteral("[Content_Types].xml"), kContentTypes)
        && zip.writeFile(QStringLiteral("xl/workbook.xml"), kWorkbook)
        && zip.writeFile(QStringLiteral("xl/_rels/workbook.xml.rels"), kWorkbookRels)
        && zip.writeFile(QStringLiteral("xl/worksheets/sheet1.xml"), sheetXml);
    if (ok && !sharedXml.isEmpty()) {
        ok = zip.writeFile(QStringLiteral("xl/sharedStrings.xml"), sharedXml);
    }
    if (!zip.close() || !ok) {
        return {};
    }
    return path;
}

void CsvXlsxBoundsTest::readsMinimalWorkbook()
{
    // Sanity for the fixture shape itself: shared and literal cells resolve.
    const QByteArray shared = QByteArrayLiteral(
        "<?xml version=\"1.0\"?>"
        "<sst xmlns=\"http://schemas.openxmlformats.org/spreadsheetml/2006/main\">"
        "<si><t>hello</t></si></sst>");
    const QString path = writeXlsx(
        QStringLiteral("minimal.xlsx"),
        wrapSheet(QByteArrayLiteral(
            "<row r=\"1\"><c r=\"A1\" t=\"s\"><v>0</v></c><c r=\"B1\"><v>42</v></c></row>")),
        shared);
    QVERIFY(!path.isEmpty());

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), 1);
    QVERIFY(!sheets.at(0).truncated);
    QCOMPARE(sheets.at(0).rows.size(), 1);
    QCOMPARE(sheets.at(0).rows.at(0),
             (QStringList{QStringLiteral("hello"), QStringLiteral("42")}));
}

void CsvXlsxBoundsTest::craftedRefStaysClamped()
{
    // "ZZZZZZ1" names column ~3.2e8; unclamped, the padding loop appends that
    // many empty QStrings (gigabytes). "ZZZZZZZZZZ1" would overflow the int
    // accumulator (UB) without the early break. Both must fall back to
    // sequential placement instead.
    const QString path = writeXlsx(
        QStringLiteral("crafted.xlsx"),
        wrapSheet(QByteArrayLiteral(
            "<row r=\"1\">"
            "<c r=\"ZZZZZZ1\"><v>1</v></c>"
            "<c r=\"ZZZZZZZZZZ1\"><v>2</v></c>"
            "<c r=\"XFE1\"><v>3</v></c>" // one past the maximum — also skipped
            "</row>")));
    QVERIFY(!path.isEmpty());

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), 1);
    QCOMPARE(sheets.at(0).rows.size(), 1);
    const QStringList &row = sheets.at(0).rows.at(0);
    QVERIFY(row.size() <= 3); // sequential, never padded toward the crafted ref
    QCOMPARE(row.at(0), QStringLiteral("1"));
}

void CsvXlsxBoundsTest::refAtColumnLimitStillHonoured()
{
    // The clamp must sit exactly at OOXML's real maximum: "XFD" (16384) is a
    // legitimate ref and still pads out; this fails if the cap is set tighter.
    const QString path = writeXlsx(
        QStringLiteral("xfd.xlsx"),
        wrapSheet(QByteArrayLiteral("<row r=\"1\"><c r=\"XFD1\"><v>9</v></c></row>")));
    QVERIFY(!path.isEmpty());

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), 1);
    QCOMPARE(sheets.at(0).rows.size(), 1);
    QCOMPARE(sheets.at(0).rows.at(0).size(), kMaxColumns);
    QCOMPARE(sheets.at(0).rows.at(0).at(kMaxColumns - 1), QStringLiteral("9"));
}

void CsvXlsxBoundsTest::oversizedSharedStringsRefused()
{
    // The zip-bomb shape: sharedStrings.xml over the part cap. The reader must
    // refuse it from the entry's declared size — the cell that references it
    // resolves empty and the sheet is flagged truncated. If the gate were
    // removed, the 33 MB string would come through and this fails.
    QByteArray shared = QByteArrayLiteral(
        "<?xml version=\"1.0\"?>"
        "<sst xmlns=\"http://schemas.openxmlformats.org/spreadsheetml/2006/main\">"
        "<si><t>");
    shared += QByteArray(static_cast<int>(kMaxPartBytes) + 1024 * 1024, 'a');
    shared += QByteArrayLiteral("</t></si></sst>");

    const QString path = writeXlsx(
        QStringLiteral("bomb.xlsx"),
        wrapSheet(QByteArrayLiteral(
            "<row r=\"1\"><c r=\"A1\" t=\"s\"><v>0</v></c></row>")),
        shared);
    QVERIFY(!path.isEmpty());

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), 1);
    QVERIFY(sheets.at(0).truncated);
    QCOMPARE(sheets.at(0).rows.size(), 1);
    QVERIFY(sheets.at(0).rows.at(0).at(0).isEmpty());
}

void CsvXlsxBoundsTest::rowCapTruncatesSheet()
{
    QByteArray data;
    data.reserve((kMaxRowsPerSheet + 10) * 7);
    for (int i = 0; i < kMaxRowsPerSheet + 10; ++i) {
        data += "<row/>";
    }
    const QString path = writeXlsx(QStringLiteral("manyrows.xlsx"), wrapSheet(data));
    QVERIFY(!path.isEmpty());

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), 1);
    QVERIFY(sheets.at(0).truncated);
    QCOMPARE(sheets.at(0).rows.size(), kMaxRowsPerSheet);
}

void CsvXlsxBoundsTest::cellCapTruncatesSheet()
{
    // 100 rows padded to the full 16384 columns each — 1.6M cells — must stop
    // at the per-sheet cell budget, well short of all 100 rows.
    QByteArray data;
    for (int i = 0; i < 100; ++i) {
        data += "<row><c r=\"XFD1\"><v>1</v></c></row>";
    }
    const QString path = writeXlsx(QStringLiteral("manycells.xlsx"), wrapSheet(data));
    QVERIFY(!path.isEmpty());

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), 1);
    QVERIFY(sheets.at(0).truncated);
    QVERIFY(sheets.at(0).rows.size() < 100);
}

void CsvXlsxBoundsTest::sheetMultiplicationStaysBounded()
{
    // The per-sheet caps multiply by the sheet count: a ~30 KB workbook.xml can
    // legally name one tiny worksheet part hundreds of times (duplicate r:id
    // targets), turning "1M cells per sheet" into hundreds of millions across
    // sheets. The workbook loop must stop at its own budget and flag the cut —
    // if kMaxSheets or the aggregate-cell cap is removed, all 500 materialise
    // and this fails.
    QByteArray wb = QByteArrayLiteral(
        "<?xml version=\"1.0\"?>"
        "<workbook xmlns=\"http://schemas.openxmlformats.org/spreadsheetml/2006/main\""
        " xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\">"
        "<sheets>");
    for (int i = 0; i < 500; ++i) {
        wb += "<sheet name=\"S" + QByteArray::number(i) + "\" sheetId=\""
            + QByteArray::number(i + 1) + "\" r:id=\"rId1\"/>";
    }
    wb += "</sheets></workbook>";

    const QString path = m_dir.filePath(QStringLiteral("multiplied.xlsx"));
    {
        KZip zip(path);
        QVERIFY(zip.open(QIODevice::WriteOnly));
        QVERIFY(zip.writeFile(QStringLiteral("[Content_Types].xml"), kContentTypes));
        QVERIFY(zip.writeFile(QStringLiteral("xl/workbook.xml"), wb));
        QVERIFY(zip.writeFile(QStringLiteral("xl/_rels/workbook.xml.rels"), kWorkbookRels));
        QVERIFY(zip.writeFile(
            QStringLiteral("xl/worksheets/sheet1.xml"),
            wrapSheet(QByteArrayLiteral("<row r=\"1\"><c r=\"A1\"><v>1</v></c></row>"))));
        QVERIFY(zip.close());
    }

    QVector<XlsxSheet> sheets;
    QVERIFY(XlsxReader::read(path, sheets));
    QCOMPARE(sheets.size(), kMaxSheets);
    QVERIFY(sheets.last().truncated);
}

void CsvXlsxBoundsTest::csvByteCapTruncates()
{
    // ~1 KB records so the byte cap trips long before the record cap.
    const QString path = m_dir.filePath(QStringLiteral("big.csv"));
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly));
        QByteArray line(1023, 'x');
        line += '\n';
        const int lines = static_cast<int>((kMaxCsvBytes + 8 * 1024 * 1024) / line.size());
        for (int i = 0; i < lines; ++i) {
            QVERIFY(f.write(line) == line.size());
        }
    }

    CsvModel m;
    QVERIFY(m.load(path));
    QVERIFY(m.byteTruncated());
    // Only the capped prefix was parsed: strictly fewer records than written.
    QVERIFY(m.rowCount() < static_cast<int>(kMaxCsvBytes / 1024));
    QVERIFY(m.rowCount() > 0);
}

void CsvXlsxBoundsTest::csvRecordCapTruncates()
{
    const QString path = m_dir.filePath(QStringLiteral("manyrecords.csv"));
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly));
        QByteArray body;
        body.reserve((kMaxCsvRecords + 100) * 4);
        for (int i = 0; i < kMaxCsvRecords + 100; ++i) {
            body += "a,b\n";
        }
        QVERIFY(f.write(body) == body.size());
    }

    CsvModel m;
    QVERIFY(m.load(path));
    QVERIFY(m.recordTruncated());
    QVERIFY(!m.byteTruncated());
    QCOMPARE(m.rowCount(), kMaxCsvRecords - 1); // first record became the header
}

void CsvXlsxBoundsTest::smallCsvIsNotTruncated()
{
    // Guards the flags against being stuck true: a normal file reports clean.
    const QString path = m_dir.filePath(QStringLiteral("small.csv"));
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly));
        f.write("a,b\n1,2\n");
    }

    CsvModel m;
    QVERIFY(m.load(path));
    QVERIFY(!m.byteTruncated());
    QVERIFY(!m.recordTruncated());
    QCOMPARE(m.rowCount(), 1);
}

void CsvXlsxBoundsTest::csvViewShowsTruncationNote()
{
    // The cap is only acceptable because it is VISIBLE: the view must carry a
    // banner saying the grid was cut, not silently show a shorter file.
    const QString path = m_dir.filePath(QStringLiteral("banner.csv"));
    {
        QFile f(path);
        QVERIFY(f.open(QIODevice::WriteOnly));
        QByteArray body;
        body.reserve((kMaxCsvRecords + 100) * 4);
        for (int i = 0; i < kMaxCsvRecords + 100; ++i) {
            body += "a,b\n";
        }
        QVERIFY(f.write(body) == body.size());
    }

    CsvView view(path);
    bool noteShown = false;
    const auto labels = view.findChildren<QLabel *>();
    for (const QLabel *label : labels) {
        if (label->isVisibleTo(&view)
            && label->text().contains(QStringLiteral("Truncated"))) {
            noteShown = true;
        }
    }
    QVERIFY(noteShown);
}

QTEST_MAIN(CsvXlsxBoundsTest)
#include "CsvXlsxBoundsTest.moc"
