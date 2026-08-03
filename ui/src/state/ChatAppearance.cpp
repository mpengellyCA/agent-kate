// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "state/ChatAppearance.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QApplication>
#include <QFontDatabase>
#include <QtMath>

namespace {
constexpr const char *kGroup = "Appearance";
constexpr const char *kDensityKey = "ChatDensity";
constexpr const char *kTextScaleKey = "ChatTextScale";

int scaled(int value, qreal factor)
{
    return qMax(1, qRound(value * factor));
}
} // namespace

ChatAppearance *ChatAppearance::instance()
{
    static auto *self = new ChatAppearance(qApp);
    return self;
}

ChatAppearance::ChatAppearance(QObject *parent)
    : QObject(parent)
{
    reload();
}

QString ChatAppearance::densityKey(Density density)
{
    switch (density) {
    case Density::Compact: return QStringLiteral("compact");
    case Density::Spacious: return QStringLiteral("spacious");
    case Density::Comfortable: return QStringLiteral("comfortable");
    }
    return QStringLiteral("comfortable");
}

ChatAppearance::Density ChatAppearance::densityFromKey(const QString &key)
{
    if (key.compare(QLatin1String("compact"), Qt::CaseInsensitive) == 0)
        return Density::Compact;
    if (key.compare(QLatin1String("spacious"), Qt::CaseInsensitive) == 0)
        return Density::Spacious;
    return Density::Comfortable;
}

int ChatAppearance::normalizedTextScale(int textScale)
{
    return qBound(-1, textScale, 1);
}

void ChatAppearance::reload()
{
    const KConfigGroup group = KSharedConfig::openConfig()->group(QString::fromLatin1(kGroup));
    set(densityFromKey(group.readEntry(kDensityKey, QStringLiteral("comfortable"))),
        normalizedTextScale(group.readEntry(kTextScaleKey, 0)), false);
}

void ChatAppearance::setDensity(Density density, bool persist)
{
    set(density, m_textScale, persist);
}

void ChatAppearance::setTextScale(int textScale, bool persist)
{
    set(m_density, textScale, persist);
}

void ChatAppearance::set(Density density, int textScale, bool writeConfig)
{
    textScale = normalizedTextScale(textScale);
    const bool changedAppearance = m_density != density || m_textScale != textScale;
    m_density = density;
    m_textScale = textScale;
    if (writeConfig)
        persist();
    if (changedAppearance) {
        ++m_generation;
        Q_EMIT changed();
    }
}

void ChatAppearance::persist()
{
    KConfigGroup group = KSharedConfig::openConfig()->group(QString::fromLatin1(kGroup));
    group.writeEntry(kDensityKey, densityKey(m_density));
    group.writeEntry(kTextScaleKey, m_textScale);
    group.sync();
}

TranscriptMetrics ChatAppearance::metrics(const QFont &applicationFont, const QPalette &palette,
                                           int viewportWidth, qreal devicePixelRatio) const
{
    Q_UNUSED(palette);
    const qreal densityFactor = m_density == Density::Compact ? 0.82
        : m_density == Density::Spacious ? 1.24 : 1.0;
    TranscriptMetrics result;
    result.outerInsetX = scaled(12, densityFactor);
    result.outerInsetY = scaled(8, densityFactor);
    result.messageGap = scaled(10, densityFactor);
    result.groupedMessageGap = scaled(3, densityFactor);
    result.bubbleRadius = scaled(10, densityFactor);
    result.messagePaddingX = scaled(12, densityFactor);
    result.messagePaddingY = scaled(9, densityFactor);
    result.messageHeaderHeight = scaled(18, densityFactor);
    result.messageHeaderGap = scaled(4, densityFactor);
    result.attachmentGap = scaled(8, densityFactor);
    result.activityGap = scaled(3, densityFactor);
    result.activityPaddingX = scaled(10, densityFactor);
    result.activityPaddingY = scaled(6, densityFactor);
    result.activityHeaderHeight = scaled(30, densityFactor);
    result.narrowBubbleWidth = scaled(480, densityFactor);
    result.activityRailWidth = scaled(3, densityFactor);
    result.attachmentTileEdge = scaled(48, densityFactor);
    result.attachmentTileDeviceEdge = qMax(1, qRound(result.attachmentTileEdge
                                                       * qMax<qreal>(1.0, devicePixelRatio)));
    result.attachmentTileHeight = scaled(44, densityFactor);
    result.attachmentTileGap = scaled(6, densityFactor);
    result.attachmentTileMaxWidth = scaled(248, densityFactor);

    result.bodyFont = applicationFont;
    result.bodyFont.setPointSizeF(qMax(6.0, applicationFont.pointSizeF() + 1.0 + m_textScale));
    result.metadataFont = result.bodyFont;
    result.metadataFont.setPointSizeF(qMax(6.0, result.bodyFont.pointSizeF() - 2.0));
    result.codeFont = QFontDatabase::systemFont(QFontDatabase::FixedFont);
    result.codeFont.setPointSizeF(result.bodyFont.pointSizeF());

    const int usable = qMax(0, viewportWidth - 2 * result.outerInsetX);
    result.assistantMaxWidth = qMin(820, usable);
    // Keep the human bubble visibly more compact than assistant prose. Its
    // width follows the available conversation pane; both speakers become
    // full-width in a narrow layout.
    result.userMaxWidth = usable <= result.narrowBubbleWidth
        ? usable : qRound(usable * 0.82);
    return result;
}
