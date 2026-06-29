// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "MarkdownUtil.h"

namespace agentkate
{
QString neutralizeMarkdownRawHtml(const QString &md)
{
    QString out;
    out.reserve(md.size() + 16);
    const int n = md.size();
    int i = 0;
    bool inFence = false;   // inside a ``` / ~~~ fenced code block
    QChar fenceChar;        // the fence delimiter in effect ('`' or '~')
    int fenceLen = 0;       // its run length (the close must be at least this)
    bool atLineStart = true;

    while (i < n) {
        // A fence opens/closes only at the start of a line (up to 3 leading
        // spaces, per CommonMark). The whole fence line is copied verbatim.
        if (atLineStart) {
            int j = i, spaces = 0;
            while (j < n && md[j] == QLatin1Char(' ') && spaces < 3) {
                ++j;
                ++spaces;
            }
            if (j < n && (md[j] == QLatin1Char('`') || md[j] == QLatin1Char('~'))) {
                const QChar fc = md[j];
                int k = j;
                while (k < n && md[k] == fc) {
                    ++k;
                }
                const int run = k - j;
                if (run >= 3) {
                    if (!inFence) {
                        inFence = true;
                        fenceChar = fc;
                        fenceLen = run;
                    } else if (fc == fenceChar && run >= fenceLen) {
                        inFence = false;
                    }
                    int eol = md.indexOf(QLatin1Char('\n'), i);
                    if (eol < 0) {
                        eol = n;
                    } else {
                        ++eol;
                    }
                    out += QStringView{md}.mid(i, eol - i);
                    i = eol;
                    atLineStart = true;
                    continue;
                }
            }
        }

        if (inFence) {
            int eol = md.indexOf(QLatin1Char('\n'), i);
            if (eol < 0) {
                eol = n;
            } else {
                ++eol;
            }
            out += QStringView{md}.mid(i, eol - i);
            i = eol;
            atLineStart = true;
            continue;
        }

        const QChar c = md[i];

        // Backslash escapes (\<, \`, ...) are markdown's own literalization; keep
        // the pair as-is so md4c produces the intended literal character.
        if (c == QLatin1Char('\\') && i + 1 < n) {
            out += c;
            out += md[i + 1];
            i += 2;
            atLineStart = false;
            continue;
        }

        // Inline code span: copy a `run`..`run` delimited region verbatim. An
        // unterminated run is just literal backticks (fall through to copy).
        if (c == QLatin1Char('`')) {
            int k = i;
            while (k < n && md[k] == QLatin1Char('`')) {
                ++k;
            }
            const int run = k - i;
            int close = -1;
            for (int p = k; p < n;) {
                if (md[p] == QLatin1Char('`')) {
                    int q = p;
                    while (q < n && md[q] == QLatin1Char('`')) {
                        ++q;
                    }
                    if (q - p == run) {
                        close = p;
                        break;
                    }
                    p = q;
                } else {
                    ++p;
                }
            }
            if (close >= 0) {
                const int endRun = close + run;
                out += QStringView{md}.mid(i, endRun - i);
                i = endRun;
                atLineStart = false;
                continue;
            }
            out += QStringView{md}.mid(i, run);
            i = k;
            atLineStart = false;
            continue;
        }

        if (c == QLatin1Char('<')) {
            out += QLatin1String("&lt;");
            ++i;
            atLineStart = false;
            continue;
        }

        out += c;
        atLineStart = (c == QLatin1Char('\n'));
        ++i;
    }
    return out;
}
} // namespace agentkate
