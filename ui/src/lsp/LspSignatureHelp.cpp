#include "LspSignatureHelp.h"

#include <QJsonArray>
#include <QJsonObject>

namespace LspSignatureHelp {

QString format(const QJsonValue &result)
{
    const QJsonObject o = result.toObject();
    const QJsonArray sigs = o.value(QStringLiteral("signatures")).toArray();
    if (sigs.isEmpty()) {
        return QString();
    }
    const int activeSig = o.value(QStringLiteral("activeSignature")).toInt(0);
    const QJsonObject sig =
        sigs.at(qBound(0, activeSig, sigs.size() - 1)).toObject();
    const QString label = sig.value(QStringLiteral("label")).toString();
    if (label.isEmpty()) {
        return QString();
    }

    const QJsonArray params = sig.value(QStringLiteral("parameters")).toArray();
    int activeParam = o.value(QStringLiteral("activeParameter")).toInt(-1);
    if (sig.contains(QStringLiteral("activeParameter"))) {
        activeParam = sig.value(QStringLiteral("activeParameter")).toInt();
    }

    // Highlight the active parameter inside the signature label when its span is
    // given as [start,end] offsets into the label.
    if (activeParam >= 0 && activeParam < params.size()) {
        const QJsonValue plabel = params.at(activeParam).toObject().value(QStringLiteral("label"));
        if (plabel.isArray()) {
            const QJsonArray span = plabel.toArray();
            const int start = span.at(0).toInt(-1);
            const int end = span.at(1).toInt(-1);
            if (start >= 0 && end > start && end <= label.size()) {
                const QString before = label.left(start).toHtmlEscaped();
                const QString mid = label.mid(start, end - start).toHtmlEscaped();
                const QString after = label.mid(end).toHtmlEscaped();
                return before + QStringLiteral("<b>") + mid + QStringLiteral("</b>") + after;
            }
        } else if (plabel.isString()) {
            const QString needle = plabel.toString();
            const int at = label.indexOf(needle);
            if (at >= 0) {
                const QString before = label.left(at).toHtmlEscaped();
                const QString mid = needle.toHtmlEscaped();
                const QString after = label.mid(at + needle.size()).toHtmlEscaped();
                return before + QStringLiteral("<b>") + mid + QStringLiteral("</b>") + after;
            }
        }
    }
    return label.toHtmlEscaped();
}

} // namespace LspSignatureHelp
