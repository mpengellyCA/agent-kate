#pragma once

#include <QImage>
#include <QSize>
#include <QString>
#include <QWidget>

class QLabel;
class QScrollArea;
class QTimer;

// ImageView displays a raster image inside an editor tab — used in place of a
// KTextEditor view when the user opens a file format that would otherwise be
// shown as decoded text garbage (PNG, JPEG, etc.). Supports fit-to-window,
// 1:1, and zoom in/out.
class ImageView : public QWidget
{
    Q_OBJECT
public:
    explicit ImageView(const QString &path, QWidget *parent = nullptr);

    QString path() const { return m_path; }
    bool isValid() const { return !m_image.isNull(); }

    static bool canDisplay(const QString &path);

private:
    QSize computeTarget() const;
    // smooth=false uses a cheap nearest-neighbour scale for live resize frames;
    // a debounced settle pass repaints once at full SmoothTransformation quality.
    void applyScale(bool smooth = true);
    void zoomBy(double factor);
    void setFitToWindow(bool on);

    void resizeEvent(QResizeEvent *event) override;

    QString m_path;
    QImage m_image;
    QLabel *m_imageLabel = nullptr;
    QScrollArea *m_scroll = nullptr;
    QTimer *m_settleTimer = nullptr;
    QSize m_lastTarget;
    bool m_lastSmooth = false;
    double m_scale = 1.0;
    bool m_fit = true;
};
