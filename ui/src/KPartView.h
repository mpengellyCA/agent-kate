#pragma once

#include <QString>
#include <QWidget>

namespace KParts {
class ReadOnlyPart;
}
class QVBoxLayout;

// KPartView embeds a read-only KDE viewer KPart inside an editor tab. Given a
// file it asks KDE "which viewer part handles this MIME type?" via
// KParts::PartLoader and hosts part->widget() exactly like TerminalPanel hosts
// the Konsole part. This single widget therefore covers PDF (Okular —
// multi-page, scroll, zoom, search, TOC), ePub/DjVu/comic books, fonts
// (KFontView), archives (Ark), and ODF/Office documents (Okular's office
// generators) — whatever parts the user has installed, with no per-format code.
//
// When no part is available, or it fails to load, the tab degrades to a clear
// "no viewer — install X" message plus an "Open externally" button, mirroring
// TerminalPanel's m_konsoleMissing fallback. The part owns its widget, so we
// delete the part in the destructor like TerminalPanel's deleteLater() teardown.
class KPartView : public QWidget
{
    Q_OBJECT
public:
    explicit KPartView(const QString &path, QWidget *parent = nullptr);
    ~KPartView() override;

    QString path() const { return m_path; }

    // True when a viewer KPart can render this file AND it isn't a type we
    // render better with a dedicated native view (text/source/markdown/csv and
    // plain raster images). Resolves the MIME by content+name via QMimeDatabase.
    static bool canDisplay(const QString &path);

private:
    void showFallback(const QString &message);

    QString m_path;
    QVBoxLayout *m_layout = nullptr;
    KParts::ReadOnlyPart *m_part = nullptr;
};
