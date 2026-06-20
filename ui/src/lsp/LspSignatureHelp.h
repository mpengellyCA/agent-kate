#pragma once

#include <QJsonValue>
#include <QString>

// LspSignatureHelp renders a textDocument/signatureHelp result into a one-line
// HTML string with the active parameter emphasised, for display in a tooltip.
namespace LspSignatureHelp {
// Returns an HTML snippet for the active signature, or an empty string when the
// result carries no signatures. The active parameter is wrapped in <b>…</b>.
QString format(const QJsonValue &result);
}
