// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "theme/ThemeManager.h"

#include <KColorScheme>
#include <KConfigGroup>
#include <KSharedConfig>

#include <QApplication>
#include <QDir>
#include <QFileInfo>
#include <QStandardPaths>

namespace {

constexpr const char *kConfigGroup = "Appearance";
constexpr const char *kConfigKey = "Theme";
constexpr const char *kKdePrefix = "kde:"; // id form for an installed .colors scheme

// Mix two colours, `t` in [0,1] toward `b`.
QColor blend(const QColor &a, const QColor &b, qreal t)
{
    return QColor::fromRgbF(a.redF() + (b.redF() - a.redF()) * t,
                            a.greenF() + (b.greenF() - a.greenF()) * t,
                            a.blueF() + (b.blueF() - a.blueF()) * t);
}

// Every QColor that a built-in Agent Kate palette needs. Active and Inactive
// groups share these; the Disabled group dims a few of them.
struct Spec {
    QColor window, windowText;
    QColor base, alternateBase, text;
    QColor button, buttonText;
    QColor brightText, placeholder;
    QColor light, midlight, mid, dark, shadow;
    QColor highlight, highlightedText;
    QColor accent, link, linkVisited;
    QColor tooltipBase, tooltipText;
    QColor disabledText, disabledBase, disabledButton;
};

QPalette buildPalette(const Spec &s)
{
    QPalette p;
    const QPalette::ColorGroup active[] = {QPalette::Active, QPalette::Inactive};
    for (QPalette::ColorGroup g : active) {
        p.setColor(g, QPalette::Window, s.window);
        p.setColor(g, QPalette::WindowText, s.windowText);
        p.setColor(g, QPalette::Base, s.base);
        p.setColor(g, QPalette::AlternateBase, s.alternateBase);
        p.setColor(g, QPalette::Text, s.text);
        p.setColor(g, QPalette::Button, s.button);
        p.setColor(g, QPalette::ButtonText, s.buttonText);
        p.setColor(g, QPalette::BrightText, s.brightText);
        p.setColor(g, QPalette::PlaceholderText, s.placeholder);
        p.setColor(g, QPalette::Light, s.light);
        p.setColor(g, QPalette::Midlight, s.midlight);
        p.setColor(g, QPalette::Mid, s.mid);
        p.setColor(g, QPalette::Dark, s.dark);
        p.setColor(g, QPalette::Shadow, s.shadow);
        p.setColor(g, QPalette::ToolTipBase, s.tooltipBase);
        p.setColor(g, QPalette::ToolTipText, s.tooltipText);
        p.setColor(g, QPalette::Link, s.link);
        p.setColor(g, QPalette::LinkVisited, s.linkVisited);
        p.setColor(g, QPalette::Accent, s.accent);
        // Inactive selection is slightly muted so an unfocused window reads.
        if (g == QPalette::Inactive) {
            p.setColor(g, QPalette::Highlight, blend(s.highlight, s.window, 0.25));
            p.setColor(g, QPalette::HighlightedText, s.highlightedText);
        } else {
            p.setColor(g, QPalette::Highlight, s.highlight);
            p.setColor(g, QPalette::HighlightedText, s.highlightedText);
        }
    }
    // Disabled group: dim text, flatten surfaces.
    p.setColor(QPalette::Disabled, QPalette::WindowText, s.disabledText);
    p.setColor(QPalette::Disabled, QPalette::Text, s.disabledText);
    p.setColor(QPalette::Disabled, QPalette::ButtonText, s.disabledText);
    p.setColor(QPalette::Disabled, QPalette::Base, s.disabledBase);
    p.setColor(QPalette::Disabled, QPalette::Window, s.window);
    p.setColor(QPalette::Disabled, QPalette::Button, s.disabledButton);
    p.setColor(QPalette::Disabled, QPalette::Highlight, blend(s.highlight, s.window, 0.55));
    p.setColor(QPalette::Disabled, QPalette::HighlightedText, s.disabledText);
    p.setColor(QPalette::Disabled, QPalette::Accent, blend(s.accent, s.window, 0.4));
    p.setColor(QPalette::Disabled, QPalette::PlaceholderText, s.disabledText);
    return p;
}

// ---- Agent Kate Midnight — the signature theme ---------------------------
// Deep navy canvas, purple raised surfaces, neon-pink accent, cool-grey text.
AkThemeDef midnightTheme()
{
    Spec s;
    s.window = QColor("#12152e");        // navy chrome
    s.windowText = QColor("#e8e8f6");    // cool near-white
    s.base = QColor("#0b0e24");          // deepest navy — editor / list canvas
    s.alternateBase = QColor("#1a1e3f"); // zebra rows / hover
    s.text = QColor("#e8e8f6");
    s.button = QColor("#1d2247");        // purple-navy raised surface
    s.buttonText = QColor("#e8e8f6");
    s.brightText = QColor("#ffffff");
    s.placeholder = QColor("#8688b3");   // muted purple-grey
    s.light = QColor("#2c3270");
    s.midlight = QColor("#252a59");
    s.mid = QColor("#2a2f63");           // dividers / borders
    s.dark = QColor("#070914");
    s.shadow = QColor("#04050d");
    s.highlight = QColor("#7c3aed");     // vivid purple selection
    s.highlightedText = QColor("#ffffff");
    s.accent = QColor("#ff2d8e");        // neon pink — focus, primary, brand
    s.link = QColor("#ff5cb8");          // pink links
    s.linkVisited = QColor("#b388ff");   // lavender
    s.tooltipBase = QColor("#1a1e3f");
    s.tooltipText = QColor("#e8e8f6");
    s.disabledText = QColor("#565a80");
    s.disabledBase = QColor("#10142c");
    s.disabledButton = QColor("#161a38");

    AkThemeDef d;
    d.id = QStringLiteral("midnight");
    d.name = QStringLiteral("Agent Kate Midnight");
    d.description = QStringLiteral("Signature dark theme — navy, purple & neon pink");
    d.kind = AkThemeDef::BuiltinPalette;
    d.dark = true;
    // Our bundled navy syntax theme (ui/themes/agent-kate-midnight.theme), so the
    // editor / diff canvas matches the navy chrome instead of Breeze-Dark grey.
    d.syntaxTheme = QStringLiteral("Agent Kate Midnight");
    d.palette = buildPalette(s);

    AkColors c;
    c.dark = true;
    c.accent = s.accent;
    c.accentText = QColor("#ffffff");
    c.positive = QColor("#34d399");      // emerald-mint
    c.negative = QColor("#fb5d6e");      // coral
    c.neutral = QColor("#fbbf24");       // amber
    c.info = QColor("#38bdf8");          // sky
    c.addedBg = QColor("#12281f");       // green tint over navy
    c.removedBg = QColor("#2c1421");     // red tint over navy
    c.hunkBg = QColor("#1b1f47");        // navy-purple
    c.agentRunning = QColor("#34d399");
    c.agentIdle = QColor("#6b6f99");
    c.lanes = {QColor("#ff2d8e"), QColor("#a855f7"), QColor("#34d399"),
               QColor("#38bdf8"), QColor("#fbbf24"), QColor("#c4b5fd")};
    d.colors = c;
    return d;
}

// ---- Agent Kate Daylight — the light companion ---------------------------
AkThemeDef daylightTheme()
{
    Spec s;
    s.window = QColor("#f3f2fb");
    s.windowText = QColor("#1c1b33");    // navy ink
    s.base = QColor("#ffffff");
    s.alternateBase = QColor("#ece9f7");
    s.text = QColor("#1c1b33");
    s.button = QColor("#eceaf8");
    s.buttonText = QColor("#1c1b33");
    s.brightText = QColor("#ffffff");
    s.placeholder = QColor("#6b6a8c");
    s.light = QColor("#ffffff");
    s.midlight = QColor("#f0eefa");
    s.mid = QColor("#cdc9e6");
    s.dark = QColor("#9c97c4");
    s.shadow = QColor("#b7b2d6");
    s.highlight = QColor("#7c3aed");
    s.highlightedText = QColor("#ffffff");
    s.accent = QColor("#d6256f");
    s.link = QColor("#c01f73");
    s.linkVisited = QColor("#7c3aed");
    s.tooltipBase = QColor("#1e1b3a");
    s.tooltipText = QColor("#ffffff");
    s.disabledText = QColor("#a6a3c4");
    s.disabledBase = QColor("#f3f2fb");
    s.disabledButton = QColor("#e7e4f3");

    AkThemeDef d;
    d.id = QStringLiteral("daylight");
    d.name = QStringLiteral("Agent Kate Daylight");
    d.description = QStringLiteral("Signature light theme — soft lilac, navy ink & pink");
    d.kind = AkThemeDef::BuiltinPalette;
    d.dark = false;
    d.syntaxTheme = QStringLiteral("Agent Kate Daylight");
    d.palette = buildPalette(s);

    AkColors c;
    c.dark = false;
    c.accent = s.accent;
    c.accentText = QColor("#ffffff");
    c.positive = QColor("#137a52");
    c.negative = QColor("#c4344a");
    c.neutral = QColor("#b7791f");
    c.info = QColor("#1f6feb");
    c.addedBg = QColor("#e4f7ee");
    c.removedBg = QColor("#fde4ea");
    c.hunkBg = QColor("#efeafb");
    c.agentRunning = QColor("#137a52");
    c.agentIdle = QColor("#9a98ba");
    c.lanes = {QColor("#d6256f"), QColor("#7c3aed"), QColor("#137a52"),
               QColor("#1f6feb"), QColor("#b7791f"), QColor("#8b5cf6")};
    d.colors = c;
    return d;
}

// Semantic colours for "follow the system scheme": derive from KColorScheme so
// the few app-specific colours track whatever palette KDE hands us.
AkColors systemColors(const QPalette &pal)
{
    AkColors c;
    c.dark = pal.color(QPalette::Base).lightness() < 128;
    const KColorScheme view(QPalette::Active, KColorScheme::View);
    const KColorScheme sel(QPalette::Active, KColorScheme::Selection);
    c.accent = pal.color(QPalette::Accent);
    if (!c.accent.isValid())
        c.accent = pal.color(QPalette::Highlight);
    c.accentText = pal.color(QPalette::HighlightedText);
    c.positive = view.foreground(KColorScheme::PositiveText).color();
    c.negative = view.foreground(KColorScheme::NegativeText).color();
    c.neutral = view.foreground(KColorScheme::NeutralText).color();
    c.info = view.foreground(KColorScheme::ActiveText).color();
    const QColor base = pal.color(QPalette::Base);
    c.addedBg = blend(base, c.positive, 0.18);
    c.removedBg = blend(base, c.negative, 0.18);
    c.hunkBg = blend(base, c.accent, 0.12);
    c.agentRunning = c.positive;
    c.agentIdle = pal.color(QPalette::Disabled, QPalette::Text);
    // Stable, theme-independent lane hues (graph identity must not shift).
    c.lanes = {QColor(0x35, 0x8c, 0xe6), QColor(0xe6, 0x7e, 0x22),
               QColor(0x2e, 0xc4, 0x71), QColor(0xe7, 0x4c, 0x3c),
               QColor(0x9b, 0x59, 0xb6), QColor(0x1a, 0xbc, 0x9c)};
    return c;
}

// Scan the standard locations for installed KDE colour schemes (*.colors).
QList<QPair<QString, QString>> discoverKdeSchemes() // (name, absolute path)
{
    QList<QPair<QString, QString>> out;
    QStringList seenNames;
    const QStringList dirs =
        QStandardPaths::locateAll(QStandardPaths::GenericDataLocation,
                                  QStringLiteral("color-schemes"),
                                  QStandardPaths::LocateDirectory);
    for (const QString &dir : dirs) {
        const QFileInfoList files =
            QDir(dir).entryInfoList({QStringLiteral("*.colors")}, QDir::Files, QDir::Name);
        for (const QFileInfo &fi : files) {
            const KSharedConfigPtr cfg = KSharedConfig::openConfig(fi.absoluteFilePath());
            const QString name =
                cfg->group(QStringLiteral("General")).readEntry("Name", fi.completeBaseName());
            if (seenNames.contains(name))
                continue; // user dir shadows system dir
            seenNames << name;
            out.append({name, fi.absoluteFilePath()});
        }
    }
    return out;
}

} // namespace

ThemeManager *ThemeManager::instance()
{
    static ThemeManager *self = new ThemeManager(qApp);
    return self;
}

QString ThemeManager::defaultId()
{
    return QStringLiteral("midnight");
}

ThemeManager::ThemeManager(QObject *parent)
    : QObject(parent)
{
    rebuildCatalog();
    // Seed with the default so colors() is valid even before applyTheme().
    m_currentId = defaultId();
    m_colors = themeById(m_currentId).colors;
}

void ThemeManager::rebuildCatalog()
{
    m_builtins.clear();
    m_builtins << midnightTheme() << daylightTheme();

    // Capture the genuine system palette once, before we ever override it, so
    // "Follow System" can restore it faithfully.
    static const QPalette systemPalette = qApp->palette();
    AkThemeDef sys;
    sys.id = QStringLiteral("system");
    sys.name = QStringLiteral("Follow System");
    sys.description = QStringLiteral("Use the desktop-wide KDE colour scheme");
    sys.kind = AkThemeDef::FollowSystem;
    sys.dark = systemPalette.color(QPalette::Base).lightness() < 128;
    sys.syntaxTheme = QString();
    sys.palette = systemPalette;
    sys.colors = systemColors(systemPalette);
    m_builtins << sys;
}

QList<AkThemeDef> ThemeManager::themes() const
{
    QList<AkThemeDef> all = m_builtins;
    for (const auto &pair : discoverKdeSchemes()) {
        AkThemeDef d;
        d.id = QString::fromLatin1(kKdePrefix) + pair.second;
        d.name = pair.first;
        d.description = QStringLiteral("Installed KDE colour scheme");
        d.kind = AkThemeDef::KdeScheme;
        d.kdeSchemeName = pair.first;
        d.builtin = false;
        const KSharedConfigPtr cfg = KSharedConfig::openConfig(pair.second);
        const QColor bg = cfg->group(QStringLiteral("Colors:View"))
                              .readEntry("BackgroundNormal", QColor(Qt::white));
        d.dark = bg.lightness() < 128;
        all << d;
    }
    return all;
}

AkThemeDef ThemeManager::themeById(const QString &id) const
{
    for (const AkThemeDef &d : m_builtins) {
        if (d.id == id)
            return d;
    }
    if (id.startsWith(QLatin1String(kKdePrefix)))
        return resolve(id);
    return resolve(defaultId());
}

AkThemeDef ThemeManager::resolve(const QString &id) const
{
    for (const AkThemeDef &d : m_builtins) {
        if (d.id == id)
            return d;
    }
    if (id.startsWith(QLatin1String(kKdePrefix))) {
        const QString path = id.mid(int(qstrlen(kKdePrefix)));
        if (QFileInfo::exists(path)) {
            const KSharedConfigPtr cfg = KSharedConfig::openConfig(path);
            AkThemeDef d;
            d.id = id;
            d.name = cfg->group(QStringLiteral("General")).readEntry("Name", QStringLiteral("KDE Scheme"));
            d.kind = AkThemeDef::KdeScheme;
            d.kdeSchemeName = d.name;
            d.builtin = false;
            d.palette = KColorScheme::createApplicationPalette(cfg);
            d.dark = d.palette.color(QPalette::Base).lightness() < 128;
            d.syntaxTheme = QString();
            d.colors = systemColors(d.palette);
            return d;
        }
    }
    // Unknown / missing → fall back to the default theme.
    for (const AkThemeDef &d : m_builtins) {
        if (d.id == defaultId())
            return d;
    }
    return midnightTheme();
}

void ThemeManager::applyTheme(const QString &id, bool persist)
{
    const AkThemeDef def = resolve(id);
    m_currentId = def.id;
    m_colors = def.colors;
    m_syntaxTheme = def.syntaxTheme;

    qApp->setPalette(def.palette);

    if (persist) {
        KConfigGroup grp = KSharedConfig::openConfig()->group(QString::fromLatin1(kConfigGroup));
        grp.writeEntry(kConfigKey, def.id);
        grp.sync();
    }
    Q_EMIT changed();
}

void ThemeManager::applySavedOrDefault()
{
    const QString id = KSharedConfig::openConfig()
                           ->group(QString::fromLatin1(kConfigGroup))
                           .readEntry(kConfigKey, defaultId());
    applyTheme(id, /*persist=*/false);
}
