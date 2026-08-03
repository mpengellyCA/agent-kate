// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QObject>
#include <QFont>
#include <QPalette>
#include <QtMath>

// TranscriptMetrics is the one place chat layout code gets density/type values.
// Values are logical pixels: Qt applies device scaling when it paints. The DPR
// is retained for bitmap consumers (attachment thumbnails) which need a device
// pixel edge rather than a second, incorrectly scaled layout system.
struct TranscriptMetrics {
    int outerInsetX = 12;
    int outerInsetY = 8;
    int messageGap = 10;
    int groupedMessageGap = 3;
    int bubbleRadius = 10;
    int messagePaddingX = 12;
    int messagePaddingY = 9;
    int messageHeaderHeight = 18;
    int messageHeaderGap = 4;
    int attachmentGap = 8;
    int narrowBubbleWidth = 480;
    int activityRailWidth = 3;
    int attachmentTileEdge = 48;
    int attachmentTileDeviceEdge = 48;
    int assistantMaxWidth = 820;
    int userMaxWidth = 0;
    QFont bodyFont;
    QFont metadataFont;
    QFont codeFont;
};

// ChatAppearance owns the persisted chat readability choices. Configuration is
// read only when this singleton is created or reload() is explicitly called;
// delegate hot paths only read its already-resolved scalar state/generation.
class ChatAppearance : public QObject
{
    Q_OBJECT
public:
    enum class Density { Compact, Comfortable, Spacious };
    Q_ENUM(Density)

    static ChatAppearance *instance();

    Density density() const { return m_density; }
    int textScale() const { return m_textScale; } // -1, 0, +1
    int generation() const { return m_generation; }

    // Derive all delegate geometry/fonts together. `viewportWidth` is in Qt
    // logical pixels and may be zero while a view is being constructed.
    TranscriptMetrics metrics(const QFont &applicationFont, const QPalette &palette,
                              int viewportWidth, qreal devicePixelRatio = 1.0) const;

    void setDensity(Density density, bool persist = true);
    void setTextScale(int textScale, bool persist = true);
    void set(Density density, int textScale, bool persist = true);
    void reload();

    static QString densityKey(Density density);
    static Density densityFromKey(const QString &key);

Q_SIGNALS:
    void changed();

private:
    explicit ChatAppearance(QObject *parent = nullptr);
    static int normalizedTextScale(int textScale);
    void persist();

    Density m_density = Density::Comfortable;
    int m_textScale = 0;
    int m_generation = 0;
};
