#pragma once

#include <QJsonObject>
#include <QObject>
#include <QString>
#include <QVariantMap>

class CoreClient;
class QWidget;

// PortalResponseWaiter listens for the org.freedesktop.portal.Request "Response"
// signal on one request object path, re-emits it as a Qt signal, then self-destructs.
// The XDG portal API is async Request/Response (a method returns a handle; the result
// arrives later on a signal), so we subscribe by predicted path BEFORE the call.
class PortalResponseWaiter : public QObject
{
    Q_OBJECT
public:
    explicit PortalResponseWaiter(const QString &requestPath, QObject *parent = nullptr);

Q_SIGNALS:
    void responded(uint code, const QVariantMap &results);

private Q_SLOTS:
    void onResponse(uint code, const QVariantMap &results);
};

// CoworkPortal services the core's `cowork.portalRequest` notifications by running
// XDG Desktop Portal operations — the Qt UI is the only process with a Wayland
// surface (plan INV-1). v1 implements `screenshot`; it returns ONLY a base64 PNG via
// `cowork.portalResult`. File descriptors / raw frames never cross the JSON bus.
class CoworkPortal : public QObject
{
    Q_OBJECT
public:
    CoworkPortal(CoreClient *core, QWidget *topLevel, QObject *parent = nullptr);

private Q_SLOTS:
    void onNotification(const QString &method, const QJsonObject &params);

private:
    void handleScreenshot(const QJsonObject &req);
    void finishScreenshot(const QString &corrId, int maxDim, const QString &format,
                          uint code, const QVariantMap &results);
    void replyResult(const QString &corrId, const QString &kind, bool ok,
                     const QString &error, const QJsonObject &extra = QJsonObject());
    QString parentWindowHandle() const;

    CoreClient *m_core = nullptr;
    QWidget *m_topLevel = nullptr;
};
