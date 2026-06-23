#include "ImageView.h"

#include <QFileInfo>
#include <QHBoxLayout>
#include <QImageReader>
#include <QLabel>
#include <QPushButton>
#include <QResizeEvent>
#include <QScrollArea>
#include <QSet>
#include <QTimer>
#include <QToolBar>
#include <QVBoxLayout>

bool ImageView::canDisplay(const QString &path)
{
    static const QSet<QByteArray> formats = [] {
        QSet<QByteArray> set;
        for (const QByteArray &fmt : QImageReader::supportedImageFormats()) {
            set.insert(fmt.toLower());
        }
        // The qpdf image plugin advertises "pdf" here, which would let this
        // static raster viewer claim PDFs and show only a flat page 1. Exclude
        // it so PDFs fall through to KPartView → Okular (multi-page, scroll,
        // search). Multi-page TIFFs degrade to page 1 the same way, but that's
        // the long-standing behaviour and QImageReader is still their best
        // native viewer, so we leave them here.
        set.remove(QByteArrayLiteral("pdf"));
        return set;
    }();
    const QByteArray suffix = QFileInfo(path).suffix().toLower().toLatin1();
    return !suffix.isEmpty() && formats.contains(suffix);
}

ImageView::ImageView(const QString &path, QWidget *parent)
    : QWidget(parent)
    , m_path(path)
{
    QImageReader reader(path);
    reader.setAutoTransform(true);
    m_image = reader.read();

    m_imageLabel = new QLabel;
    m_imageLabel->setAlignment(Qt::AlignCenter);
    m_imageLabel->setBackgroundRole(QPalette::Base);

    m_scroll = new QScrollArea;
    m_scroll->setBackgroundRole(QPalette::Base);
    m_scroll->setWidget(m_imageLabel);
    m_scroll->setWidgetResizable(false);
    m_scroll->setAlignment(Qt::AlignCenter);

    // Coalesce the storm of resize events from a splitter/window drag: each
    // frame gets a cheap nearest-neighbour rescale (computed in resizeEvent),
    // and this one-shot fires ~80 ms after the drag settles to repaint the
    // final size once at full smooth quality. Smooth-scaling a full-resolution
    // source on every event is the doc-view resize stall this avoids.
    m_settleTimer = new QTimer(this);
    m_settleTimer->setSingleShot(true);
    m_settleTimer->setInterval(80);
    connect(m_settleTimer, &QTimer::timeout, this, [this] {
        if (m_fit) {
            applyScale(true);
        }
    });

    auto *toolbar = new QToolBar;
    toolbar->setIconSize(QSize(16, 16));

    auto *info = new QLabel;
    if (m_image.isNull()) {
        info->setText(tr("Failed to decode %1").arg(QFileInfo(path).fileName()));
    } else {
        info->setText(QStringLiteral("%1 × %2").arg(m_image.width()).arg(m_image.height()));
    }
    info->setStyleSheet(QStringLiteral("color: palette(mid); padding: 0 8px;"));

    auto *fitAction = toolbar->addAction(tr("Fit"));
    fitAction->setCheckable(true);
    fitAction->setChecked(true);
    connect(fitAction, &QAction::toggled, this, [this](bool on) { setFitToWindow(on); });

    auto *actualAction = toolbar->addAction(tr("1:1"));
    connect(actualAction, &QAction::triggered, this, [this, fitAction] {
        fitAction->setChecked(false);
        m_fit = false;
        m_scale = 1.0;
        applyScale();
    });

    auto *zoomOut = toolbar->addAction(QStringLiteral("−"));
    connect(zoomOut, &QAction::triggered, this, [this, fitAction] {
        fitAction->setChecked(false);
        zoomBy(1.0 / 1.25);
    });
    auto *zoomIn = toolbar->addAction(QStringLiteral("+"));
    connect(zoomIn, &QAction::triggered, this, [this, fitAction] {
        fitAction->setChecked(false);
        zoomBy(1.25);
    });

    toolbar->addSeparator();
    toolbar->addWidget(info);

    auto *layout = new QVBoxLayout(this);
    layout->setContentsMargins(0, 0, 0, 0);
    layout->setSpacing(0);
    layout->addWidget(toolbar);
    layout->addWidget(m_scroll, 1);

    if (m_image.isNull()) {
        m_imageLabel->setText(tr("Cannot display image: %1").arg(QFileInfo(path).fileName()));
        m_imageLabel->resize(m_imageLabel->sizeHint());
    } else {
        applyScale();
    }
}

void ImageView::setFitToWindow(bool on)
{
    m_fit = on;
    applyScale();
}

void ImageView::zoomBy(double factor)
{
    m_fit = false;
    m_scale = qBound(0.05, m_scale * factor, 32.0);
    applyScale();
}

QSize ImageView::computeTarget() const
{
    if (m_image.isNull()) {
        return {};
    }
    if (m_fit) {
        const QSize avail = m_scroll->viewport()->size();
        const QSize target = m_image.size().scaled(avail, Qt::KeepAspectRatio);
        return target.isEmpty() ? m_image.size() : target;
    }
    return m_image.size() * m_scale;
}

void ImageView::applyScale(bool smooth)
{
    if (m_image.isNull()) {
        return;
    }
    const QSize target = computeTarget();
    m_imageLabel->setPixmap(QPixmap::fromImage(m_image.scaled(
        target, Qt::KeepAspectRatio,
        smooth ? Qt::SmoothTransformation : Qt::FastTransformation)));
    m_imageLabel->resize(target);
    m_lastTarget = target;
    m_lastSmooth = smooth;
}

void ImageView::resizeEvent(QResizeEvent *event)
{
    QWidget::resizeEvent(event);
    if (!m_fit) {
        return;
    }
    // Nothing to do if the fitted size is unchanged and already smooth (e.g. a
    // resize that didn't alter the constraining dimension).
    if (computeTarget() == m_lastTarget && m_lastSmooth) {
        return;
    }
    applyScale(false);        // cheap immediate frame
    m_settleTimer->start();   // one smooth repaint once the drag stops
}
