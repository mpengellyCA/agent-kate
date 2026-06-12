#include "ImageView.h"

#include <QFileInfo>
#include <QHBoxLayout>
#include <QImageReader>
#include <QLabel>
#include <QPushButton>
#include <QResizeEvent>
#include <QScrollArea>
#include <QSet>
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

void ImageView::applyScale()
{
    if (m_image.isNull()) {
        return;
    }
    QSize target;
    if (m_fit) {
        const QSize avail = m_scroll->viewport()->size();
        target = m_image.size().scaled(avail, Qt::KeepAspectRatio);
        if (target.isEmpty()) {
            target = m_image.size();
        }
    } else {
        target = m_image.size() * m_scale;
    }
    m_imageLabel->setPixmap(QPixmap::fromImage(
        m_image.scaled(target, Qt::KeepAspectRatio, Qt::SmoothTransformation)));
    m_imageLabel->resize(target);
}

void ImageView::resizeEvent(QResizeEvent *event)
{
    QWidget::resizeEvent(event);
    if (m_fit) {
        applyScale();
    }
}
