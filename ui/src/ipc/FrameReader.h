#pragma once

#include <QByteArray>

#include <utility>

// Inbound framing for the core connection, with the same cap the core enforces
// on its own inbound side (core/internal/ipc/server.go readFrame).
//
// Without a cap the UI's read buffer is whatever the core sends: one
// newline-terminated frame with no newline in sight grows m_buf without bound,
// and a wedged or hostile writer walks the GUI process into the OOM killer
// (audit F10). The cap is NOT a connection kill: an over-long frame costs that
// frame only, exactly as on the core side, because dropping the link would take
// every other agent's feed down with it.
//
// Header-only on purpose: CoreClient.cpp is compiled into four different test
// binaries, and a new .cpp would have to be added to every one of them.
namespace akipc {

// Matches maxFrameBytes in core/internal/ipc/server.go. The two ends must agree
// or one of them silently accepts what the other refuses.
constexpr qsizetype kMaxInboundFrameBytes = 16 * 1024 * 1024;

// How much of an oversize frame's head is retained. Long enough to carry the
// JSON-RPC id of any real reply (it is written near the front of the object),
// so a caller waiting on it can be failed now instead of leaking its callback
// forever. Nothing past this is kept — only counted.
constexpr qsizetype kIdProbeBytes = 512;

// Buffer capacity kept between frames. One giant frame otherwise leaves its
// array attached to the reader for the life of the process.
constexpr qsizetype kRetainedBufferBytes = 1 << 20;

class FrameReader
{
public:
    struct Frame {
        // A complete, in-cap frame (EOL stripped). Empty when oversize > 0.
        QByteArray line;
        // Bytes discarded, when this "frame" was over the cap. 0 otherwise.
        qint64 oversize = 0;
        // The retained head of a discarded frame, for id recovery.
        QByteArray probe;
    };

    void append(const QByteArray &data) { m_buf.append(data); }

    // Pops the next frame. Returns false when the buffer holds no complete
    // frame yet (the caller waits for more bytes).
    //
    // Blank lines are consumed and skipped here rather than handed up: they
    // carry nothing and every caller would drop them anyway.
    bool next(Frame *out)
    {
        for (;;) {
            const qsizetype nl = m_buf.indexOf('\n');
            if (m_dropping) {
                if (nl < 0) {
                    // Still no end in sight: count and discard everything we
                    // hold. Memory stays flat however long the line runs.
                    m_dropped += m_buf.size();
                    reset();
                    return false;
                }
                m_dropped += nl + 1;
                m_buf.remove(0, nl + 1);
                m_dropping = false;
                emitDrop(out);
                return true;
            }
            if (nl < 0) {
                if (m_buf.size() > kMaxInboundFrameBytes) {
                    // A partial line already past the cap. It can only get
                    // longer, so stop accumulating now — waiting for the
                    // newline first is the unbounded growth we are preventing.
                    beginDrop();
                    continue;
                }
                return false; // partial frame; wait for the rest
            }
            if (nl > kMaxInboundFrameBytes) {
                m_probe = m_buf.left(kIdProbeBytes);
                m_dropped = nl + 1;
                m_buf.remove(0, nl + 1);
                trim();
                emitDrop(out);
                return true;
            }
            QByteArray line = m_buf.left(nl);
            m_buf.remove(0, nl + 1);
            trim();
            if (line.trimmed().isEmpty()) {
                continue;
            }
            out->line = std::move(line);
            out->oversize = 0;
            out->probe.clear();
            return true;
        }
    }

    qsizetype buffered() const { return m_buf.size(); }

    // Forget everything, including a drop in progress. Used when the connection
    // goes down: a half-frame left mid-stream — or a discard the newline for
    // which never arrived — must not bleed into the next connection.
    void clear()
    {
        reset();
        m_probe.clear();
        m_dropped = 0;
        m_dropping = false;
    }

private:
    void beginDrop()
    {
        m_probe = m_buf.left(kIdProbeBytes);
        m_dropped = m_buf.size();
        m_dropping = true;
        reset();
    }

    void emitDrop(Frame *out)
    {
        out->line.clear();
        out->oversize = m_dropped;
        out->probe = m_probe;
        m_dropped = 0;
        m_probe.clear();
    }

    // Release a one-off giant frame's array rather than carry it for the life
    // of the connection.
    void trim()
    {
        if (m_buf.capacity() > kRetainedBufferBytes) {
            m_buf.squeeze();
        }
    }

    void reset()
    {
        m_buf.clear();
        m_buf.squeeze();
    }

    QByteArray m_buf;
    QByteArray m_probe;
    qint64 m_dropped = 0;
    bool m_dropping = false;
};

} // namespace akipc
