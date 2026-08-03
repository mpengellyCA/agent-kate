// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "state/ChatAppearance.h"
#include "theme/ThemeManager.h"

#include <KConfigGroup>
#include <KSharedConfig>

#include <QSignalSpy>
#include <QDir>
#include <QStandardPaths>
#include <QtTest>

class ChatAppearanceTest : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void initTestCase();
    void defaultsAndInvalidStoredValues();
    void liveChangesNotifyAndPersist();
    void metricsFollowDensityAndTextScale();
    void builtinTranscriptTokensMeetContrast();
};

void ChatAppearanceTest::initTestCase()
{
    QStandardPaths::setTestModeEnabled(true);
    // Keep this stateful preference test out of the developer's real
    // agentkaterc and away from any concurrently-running app instance.
    KSharedConfig::setMainConfigName(QDir::tempPath() + QStringLiteral("/chatappearance-testrc"));
    KConfigGroup appearance = KSharedConfig::openConfig()->group(QStringLiteral("Appearance"));
    appearance.deleteEntry(QStringLiteral("ChatDensity"));
    appearance.deleteEntry(QStringLiteral("ChatTextScale"));
    appearance.sync();
    ChatAppearance::instance()->reload();
}

void ChatAppearanceTest::defaultsAndInvalidStoredValues()
{
    KConfigGroup appearance = KSharedConfig::openConfig()->group(QStringLiteral("Appearance"));
    appearance.deleteEntry(QStringLiteral("ChatDensity"));
    appearance.deleteEntry(QStringLiteral("ChatTextScale"));
    appearance.sync();
    ChatAppearance::instance()->reload();
    QCOMPARE(ChatAppearance::instance()->density(), ChatAppearance::Density::Comfortable);
    QCOMPARE(ChatAppearance::instance()->textScale(), 0);

    appearance.writeEntry("ChatDensity", QStringLiteral("not-a-density"));
    appearance.writeEntry("ChatTextScale", 99);
    appearance.sync();
    ChatAppearance::instance()->reload();
    QCOMPARE(ChatAppearance::instance()->density(), ChatAppearance::Density::Comfortable);
    QCOMPARE(ChatAppearance::instance()->textScale(), 1);
}

void ChatAppearanceTest::liveChangesNotifyAndPersist()
{
    auto *appearance = ChatAppearance::instance();
    appearance->set(ChatAppearance::Density::Comfortable, 0, false);
    const int before = appearance->generation();
    QSignalSpy changed(appearance, &ChatAppearance::changed);

    appearance->set(ChatAppearance::Density::Spacious, 1, true);
    QCOMPARE(changed.count(), 1);
    QVERIFY(appearance->generation() > before);
    QCOMPARE(KSharedConfig::openConfig()->group(QStringLiteral("Appearance"))
                 .readEntry("ChatDensity", QString()), QStringLiteral("spacious"));
    QCOMPARE(KSharedConfig::openConfig()->group(QStringLiteral("Appearance"))
                 .readEntry("ChatTextScale", 0), 1);

    appearance->set(ChatAppearance::Density::Spacious, 1, false);
    QCOMPARE(changed.count(), 1);
}

void ChatAppearanceTest::metricsFollowDensityAndTextScale()
{
    auto *appearance = ChatAppearance::instance();
    const QFont appFont;
    const QPalette palette;
    appearance->set(ChatAppearance::Density::Compact, -1, false);
    const TranscriptMetrics compact = appearance->metrics(appFont, palette, 1000, 2.0);
    appearance->set(ChatAppearance::Density::Spacious, 1, false);
    const TranscriptMetrics spacious = appearance->metrics(appFont, palette, 1000, 2.0);

    QVERIFY(spacious.messageGap > compact.messageGap);
    QVERIFY(spacious.attachmentTileEdge > compact.attachmentTileEdge);
    QVERIFY(spacious.bodyFont.pointSizeF() > compact.bodyFont.pointSizeF());
    QCOMPARE(spacious.assistantMaxWidth, 820);
    QCOMPARE(spacious.userMaxWidth, qRound((1000 - 2 * spacious.outerInsetX) * 0.82));
    QCOMPARE(spacious.attachmentTileDeviceEdge, spacious.attachmentTileEdge * 2);
}

void ChatAppearanceTest::builtinTranscriptTokensMeetContrast()
{
    for (const QString &id : {QStringLiteral("midnight"), QStringLiteral("daylight")}) {
        const AkThemeDef def = ThemeManager::instance()->themeById(id);
        const QColor body = def.palette.color(QPalette::Text);
        const AkColors &c = def.colors;
        const QList<QColor> surfaces = {c.chatAssistantSurface, c.chatUserSurface,
                                        c.chatActivitySurface, c.chatCodeSurface,
                                        c.chatAttachmentSurface};
        for (const QColor &surface : surfaces) {
            QVERIFY2(akColorContrastRatio(body, surface) >= 4.5,
                     qPrintable(id + QStringLiteral(": body text contrast")));
            QVERIFY2(akColorContrastRatio(c.chatMetadata, surface) >= 4.5,
                     qPrintable(id + QStringLiteral(": metadata contrast")));
        }
        QVERIFY2(akColorContrastRatio(c.chatRail, c.chatActivitySurface) >= 3.0,
                 qPrintable(id + QStringLiteral(": activity rail contrast")));
        QVERIFY2(akColorContrastRatio(c.chatBorder, c.chatAssistantSurface) >= 3.0,
                 qPrintable(id + QStringLiteral(": assistant border contrast")));
    }
}

QTEST_MAIN(ChatAppearanceTest)
#include "ChatAppearanceTest.moc"
