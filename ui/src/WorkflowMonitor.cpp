// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "WorkflowMonitor.h"

#include "SafeContent.h"

#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QFileSystemWatcher>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QMap>
#include <QRegularExpression>
#include <QTimer>

namespace {

// Poll cadence while a workflow is still running — a backstop for the file
// watcher, which coalesces and can miss rapid append bursts.
constexpr int kPollMs = 1500;
// After this long with no snapshot change, back the poll off to kIdlePollMs.
// Never stop outright for a non-terminal run: the backed-off poll (plus the
// watcher) is what lets the view recover when activity resumes.
constexpr qint64 kIdleAfterMs = 10 * 60 * 1000;
constexpr int kIdlePollMs = 30 * 1000;
// Coalesce a flurry of watcher signals into one rescan.
constexpr int kDebounceMs = 250;
// How much of each sub-agent transcript to read from the end for "current
// activity". Enough to hold the last few stream-json events.
constexpr qint64 kTailBytes = 64 * 1024;
// Most new journal bytes one poll will ingest. A faster writer is skipped
// forward (readBoundedTail's gap), never read unboundedly.
constexpr qint64 kJournalChunkBytes = 256 * 1024;
// A journal entry is a one-line JSON record; a "line" still unterminated past
// this is not one, so the carried partial is dropped rather than hoarded.
constexpr qint64 kMaxJournalLineBytes = 64 * 1024;
// The final <runId>.json is written into an agent-writable tree, so its size is
// attacker-influenced: bound the read instead of readAll (audit F55 class).
constexpr qint64 kMaxFinalJsonBytes = 4 * 1024 * 1024;

// Pull the first capture of `pattern` out of `text`, trimmed, or empty.
QString firstMatch(const QString &text, const QString &pattern)
{
    const QRegularExpression re(pattern);
    const QRegularExpressionMatch m = re.match(text);
    return m.hasMatch() ? m.captured(1).trimmed() : QString();
}

// A compact one-line summary of a tool_use block for the activity column.
QString summarizeToolUse(const QString &name, const QJsonObject &input)
{
    QString detail;
    // Prefer the fields that read well at a glance, per tool.
    for (const char *key : {"command", "description", "file_path", "pattern",
                            "prompt", "path", "summary"}) {
        const QString v = input.value(QLatin1String(key)).toString().simplified();
        if (!v.isEmpty()) {
            detail = v;
            break;
        }
    }
    if (detail.isEmpty()) {
        return name;
    }
    if (detail.size() > 80) {
        detail = detail.left(79) + QChar(0x2026);
    }
    return name + QStringLiteral(": ") + detail;
}

} // namespace

WorkflowMonitor::WorkflowMonitor(const QString &inputJson, const QString &resultText,
                                 QObject *parent)
    : QObject(parent)
{
    parseAnchors(resultText);
    parseScriptPhases(inputJson);
    m_snapshot.runId = m_runId;
    m_snapshot.taskId = m_taskId;
    m_snapshot.scriptFile = m_scriptFile;
    m_snapshot.transcriptDir = m_transcriptDir;
    for (const auto &p : m_scriptPhases) {
        m_snapshot.planPhases << p.first;
    }
    if (isValid()) {
        startWatching();
        refresh();
    }
}

WorkflowMonitor::~WorkflowMonitor() = default;

void WorkflowMonitor::parseAnchors(const QString &resultText)
{
    m_taskId = firstMatch(resultText, QStringLiteral("Task ID:\\s*(\\S+)"));
    m_runId = firstMatch(resultText, QStringLiteral("Run ID:\\s*(\\S+)"));
    m_scriptFile = firstMatch(resultText, QStringLiteral("Script file:\\s*(\\S+)"));
    m_transcriptDir = firstMatch(resultText, QStringLiteral("Transcript dir:\\s*(\\S+)"));

    if (m_transcriptDir.isEmpty()) {
        return;
    }
    // The rich final snapshot lives beside the subagents tree:
    //   …/subagents/workflows/<runId>  ->  …/workflows/<runId>.json
    QString base = m_transcriptDir;
    base.replace(QStringLiteral("/subagents/workflows/"),
                 QStringLiteral("/workflows/"));
    m_finalJsonPath = base + QStringLiteral(".json");
}

void WorkflowMonitor::parseScriptPhases(const QString &inputJson)
{
    const QJsonObject input = QJsonDocument::fromJson(inputJson.toUtf8()).object();
    const QString script = input.value(QStringLiteral("script")).toString();
    if (script.isEmpty()) {
        return;
    }
    // The script is JS, not JSON, so meta.phases can't be parsed structurally.
    // Isolate the first `phases: [ … ]` block (the meta literal) by bracket
    // matching, then pull each object's title/detail string literals. Tolerant
    // and best-effort: an empty result is fine (the live view just omits the plan).
    const int at = script.indexOf(QStringLiteral("phases"));
    if (at < 0) {
        return;
    }
    const int open = script.indexOf(QLatin1Char('['), at);
    if (open < 0) {
        return;
    }
    int depth = 0;
    int close = -1;
    for (int i = open; i < script.size(); ++i) {
        const QChar c = script.at(i);
        if (c == QLatin1Char('[')) {
            ++depth;
        } else if (c == QLatin1Char(']')) {
            if (--depth == 0) {
                close = i;
                break;
            }
        }
    }
    if (close < 0) {
        return;
    }
    const QString block = script.mid(open, close - open + 1);
    // Each phase entry: { title: '…', detail: '…' } (quotes may be ' or ").
    const QRegularExpression titleRe(
        QStringLiteral("title:\\s*(['\"])(.*?)\\1"));
    const QRegularExpression detailRe(
        QStringLiteral("detail:\\s*(['\"])(.*?)\\1"));
    // Split into object chunks on '}' so title/detail stay paired within a phase.
    const QStringList chunks = block.split(QLatin1Char('}'), Qt::SkipEmptyParts);
    for (const QString &chunk : chunks) {
        const QRegularExpressionMatch tm = titleRe.match(chunk);
        if (!tm.hasMatch()) {
            continue;
        }
        const QString title = tm.captured(2).trimmed();
        const QRegularExpressionMatch dm = detailRe.match(chunk);
        const QString detail = dm.hasMatch() ? dm.captured(2).trimmed() : QString();
        if (!title.isEmpty()) {
            m_scriptPhases.append({title, detail});
        }
    }
}

void WorkflowMonitor::startWatching()
{
    m_debounce = new QTimer(this);
    m_debounce->setSingleShot(true);
    m_debounce->setInterval(kDebounceMs);
    connect(m_debounce, &QTimer::timeout, this, &WorkflowMonitor::refresh);

    m_watcher = new QFileSystemWatcher(this);
    const auto kick = [this] {
        if (m_debounce && !m_debounce->isActive()) {
            m_debounce->start();
        }
    };
    connect(m_watcher, &QFileSystemWatcher::directoryChanged, this, kick);
    connect(m_watcher, &QFileSystemWatcher::fileChanged, this, kick);
    // Watch the live transcript dir (journal + agent files) and the sibling
    // workflows dir where the final <runId>.json appears on completion. Missing
    // paths are simply skipped; the poll timer covers the not-yet-created case.
    if (QFileInfo::exists(m_transcriptDir)) {
        m_watcher->addPath(m_transcriptDir);
    }
    const QString workflowsDir = QFileInfo(m_finalJsonPath).absolutePath();
    if (QFileInfo::exists(workflowsDir)) {
        m_watcher->addPath(workflowsDir);
    }

    m_poll = new QTimer(this);
    m_poll->setInterval(kPollMs);
    connect(m_poll, &QTimer::timeout, this, &WorkflowMonitor::refresh);
    m_sinceChange.start();
    m_poll->start();
}

void WorkflowMonitor::refresh()
{
    if (!isValid()) {
        return;
    }
    Snapshot s;
    // Re-attach the watcher to the transcript dir once it exists (it may not have
    // when the monitor was created milliseconds after launch).
    const bool dirExists = QFileInfo::exists(m_transcriptDir);
    if (dirExists) {
        m_sawTranscriptDir = true;
        if (m_watcher && !m_watcher->directories().contains(m_transcriptDir)) {
            m_watcher->addPath(m_transcriptDir);
        }
    }

    // Regular files only, and never more than the cap: the final json lives in
    // an agent-writable tree. An over-cap file parses as truncated JSON, fails,
    // and falls through to the live view — refuse rather than readAll.
    const QFileInfo finalInfo(m_finalJsonPath);
    if (finalInfo.isFile() && finalInfo.size() <= kMaxFinalJsonBytes) {
        QFile finalFile(m_finalJsonPath);
        if (finalFile.open(QIODevice::ReadOnly)) {
            const QJsonObject root =
                QJsonDocument::fromJson(finalFile.read(kMaxFinalJsonBytes)).object();
            finalFile.close();
            if (!root.isEmpty()) {
                s = buildFromFinalJson(root);
            }
        }
    }
    if (s.state == State::Unknown) {
        if (m_sawTranscriptDir && !dirExists) {
            // The live tree existed and is gone, with no final snapshot: the
            // run died (or was cleaned up) mid-flight. Terminal — stop polling
            // rather than re-scanning a void forever.
            s.state = State::Failed;
            s.summary = QStringLiteral(
                "Transcript directory disappeared before a final result was written.");
            s.phases = m_snapshot.phases;
            s.agentCount = m_snapshot.agentCount;
        } else {
            s = buildFromLive();
        }
    }

    // Carry the launch anchors + plan through every snapshot.
    s.runId = m_runId;
    s.taskId = m_taskId;
    s.scriptFile = m_scriptFile;
    s.transcriptDir = m_transcriptDir;
    if (s.planPhases.isEmpty()) {
        for (const auto &p : m_scriptPhases) {
            s.planPhases << p.first;
        }
    }

    const QString fp = fingerprint(s);
    if (fp == m_fingerprint) {
        // Long quiet spell: drop the poll to the idle cadence. The watcher and
        // the idle poll both still call back here, and any change resets the
        // cadence below — the view recovers, it just stops burning 1.5 s scans.
        if (m_poll && m_poll->isActive() && m_sinceChange.isValid()
            && m_sinceChange.elapsed() >= kIdleAfterMs) {
            m_poll->setInterval(kIdlePollMs);
        }
        return;
    }
    m_fingerprint = fp;
    m_snapshot = s;
    m_sinceChange.restart();
    if (m_poll && m_poll->interval() != kPollMs) {
        m_poll->setInterval(kPollMs);
    }
    emit changed();

    // Once terminal, stop polling — the final json won't change again.
    if (m_poll && (s.state == State::Completed || s.state == State::Failed)) {
        m_poll->stop();
    }
}

WorkflowMonitor::Snapshot
WorkflowMonitor::buildFromFinalJson(const QJsonObject &root) const
{
    Snapshot s;
    const QString status = root.value(QStringLiteral("status")).toString();
    s.state = (status == QLatin1String("failed") || status == QLatin1String("error"))
                  ? State::Failed
                  : State::Completed;
    s.summary = root.value(QStringLiteral("summary")).toString();
    s.agentCount = root.value(QStringLiteral("agentCount")).toInt();
    s.totalTokens =
        static_cast<qint64>(root.value(QStringLiteral("totalTokens")).toDouble());
    s.totalToolCalls = root.value(QStringLiteral("totalToolCalls")).toInt();
    s.durationMs =
        static_cast<qint64>(root.value(QStringLiteral("durationMs")).toDouble());

    for (const QJsonValue &lv : root.value(QStringLiteral("logs")).toArray()) {
        const QString line = lv.toString().trimmed();
        if (!line.isEmpty()) {
            s.logs << line;
        }
    }

    // Seed phase groups (in declared order) from the phases array so empty phases
    // still appear, then attach agents from workflowProgress by phaseTitle.
    QMap<QString, int> phaseIndex; // title -> position in s.phases
    for (const QJsonValue &pv : root.value(QStringLiteral("phases")).toArray()) {
        const QJsonObject po = pv.toObject();
        Phase phase;
        phase.title = po.value(QStringLiteral("title")).toString();
        phase.detail = po.value(QStringLiteral("detail")).toString();
        phaseIndex.insert(phase.title, s.phases.size());
        s.phases.append(phase);
    }

    const auto phaseFor = [&](const QString &title) -> Phase & {
        const QString key = title.isEmpty() ? QStringLiteral("Agents") : title;
        auto it = phaseIndex.constFind(key);
        if (it != phaseIndex.constEnd()) {
            return s.phases[it.value()];
        }
        Phase phase;
        phase.title = key;
        phaseIndex.insert(key, s.phases.size());
        s.phases.append(phase);
        return s.phases.last();
    };

    for (const QJsonValue &wv : root.value(QStringLiteral("workflowProgress")).toArray()) {
        const QJsonObject w = wv.toObject();
        if (w.value(QStringLiteral("type")).toString() != QLatin1String("workflow_agent")) {
            continue;
        }
        SubAgent a;
        a.agentId = w.value(QStringLiteral("agentId")).toString();
        a.label = w.value(QStringLiteral("label")).toString();
        a.model = w.value(QStringLiteral("model")).toString();
        a.state = w.value(QStringLiteral("state")).toString();
        a.tokens = static_cast<qint64>(w.value(QStringLiteral("tokens")).toDouble());
        a.toolCalls = w.value(QStringLiteral("toolCalls")).toInt();
        a.lastActivity = w.value(QStringLiteral("lastToolSummary")).toString().simplified();
        a.promptPreview = w.value(QStringLiteral("promptPreview")).toString();
        a.resultPreview = w.value(QStringLiteral("resultPreview")).toString();
        if (!a.agentId.isEmpty() && !m_transcriptDir.isEmpty()) {
            a.jsonlPath = m_transcriptDir + QStringLiteral("/agent-") + a.agentId
                          + QStringLiteral(".jsonl");
        }
        phaseFor(w.value(QStringLiteral("phaseTitle")).toString()).agents.append(a);
    }
    return s;
}

bool WorkflowMonitor::pollJournal(JournalState &st, const QString &path)
{
    const agentkate::TailRead chunk =
        agentkate::readBoundedTail(path, st.offset, kJournalChunkBytes);
    applyJournalChunk(st, chunk);
    return chunk.restarted;
}

void WorkflowMonitor::applyJournalChunk(JournalState &st,
                                        const agentkate::TailRead &chunk)
{
    if (chunk.restarted) {
        // Truncated or rewritten underneath us: everything derived from the old
        // bytes no longer describes this file.
        st.remainder.clear();
        st.order.clear();
        st.done.clear();
    }
    QByteArray buf;
    if (chunk.gap) {
        // We jumped over bytes, so the carried partial no longer joins up and
        // the window almost certainly begins mid-line: resync to the first
        // newline. Entries in the skipped span are simply missed (best-effort).
        st.remainder.clear();
        const int nl = chunk.bytes.indexOf('\n');
        if (nl < 0) {
            return;
        }
        buf = chunk.bytes.mid(nl + 1);
    } else {
        buf = st.remainder + chunk.bytes;
        st.remainder.clear();
    }
    int start = 0;
    while (true) {
        const int nl = buf.indexOf('\n', start);
        if (nl < 0) {
            break;
        }
        const QByteArray line = buf.mid(start, nl - start);
        start = nl + 1;
        const QJsonObject o = QJsonDocument::fromJson(line).object();
        const QString agentId = o.value(QStringLiteral("agentId")).toString();
        if (agentId.isEmpty()) {
            continue;
        }
        if (!st.done.contains(agentId)) {
            st.order.append(agentId);
            st.done.insert(agentId, false);
        }
        if (o.value(QStringLiteral("type")).toString() == QLatin1String("result")) {
            st.done.insert(agentId, true);
        }
    }
    st.remainder = buf.mid(start);
    if (st.remainder.size() > kMaxJournalLineBytes) {
        st.remainder.clear();
    }
}

WorkflowMonitor::Snapshot WorkflowMonitor::buildFromLive()
{
    Snapshot s;
    s.state = State::Running;

    // Fold in only the bytes appended since the last poll; the accumulated
    // order/done state persists across refreshes. A shrunk journal resets it,
    // and the per-agent tail cache with it. (A missing journal — dir not
    // created yet — is still a valid running state: just the plan to show.)
    if (pollJournal(m_journal, m_transcriptDir + QStringLiteral("/journal.jsonl"))) {
        m_agentTails.clear();
    }

    Phase running;
    running.title = QStringLiteral("Running");
    Phase finished;
    finished.title = QStringLiteral("Completed");

    for (const QString &agentId : m_journal.order) {
        SubAgent a;
        a.agentId = agentId;
        a.label = QStringLiteral("agent ") + agentId.left(8);
        a.jsonlPath = m_transcriptDir + QStringLiteral("/agent-") + agentId
                      + QStringLiteral(".jsonl");
        const bool isDone = m_journal.done.value(agentId);
        a.state = isDone ? QStringLiteral("done") : QStringLiteral("running");
        // Re-tail only when the transcript moved since the last poll — size and
        // mtime together are the cheap "did anything change" signal.
        AgentTail &tail = m_agentTails[agentId];
        const QFileInfo fi(a.jsonlPath);
        if (fi.exists() && (fi.size() != tail.size || fi.lastModified() != tail.mtime)) {
            tail.size = fi.size();
            tail.mtime = fi.lastModified();
            QString preview;
            tail.lastActivity = tailActivity(a.jsonlPath, preview);
            tail.preview = preview;
        }
        a.lastActivity = tail.lastActivity;
        a.resultPreview = tail.preview;
        (isDone ? finished : running).agents.append(a);
    }
    s.agentCount = m_journal.order.size();
    if (!running.agents.isEmpty()) {
        s.phases.append(running);
    }
    if (!finished.agents.isEmpty()) {
        s.phases.append(finished);
    }
    return s;
}

QString WorkflowMonitor::tailActivity(const QString &jsonlPath, QString &preview)
{
    QFile f(jsonlPath);
    if (!f.open(QIODevice::ReadOnly)) {
        return QString();
    }
    const qint64 size = f.size();
    if (size > kTailBytes) {
        f.seek(size - kTailBytes);
        f.readLine(); // drop the partial first line after seeking mid-file
    }

    QString lastActivity;
    while (!f.atEnd()) {
        const QByteArray line = f.readLine();
        if (line.isEmpty()) {
            continue;
        }
        const QJsonObject o = QJsonDocument::fromJson(line).object();
        const QJsonObject msg = o.value(QStringLiteral("message")).toObject();
        if (msg.value(QStringLiteral("role")).toString() != QLatin1String("assistant")) {
            continue;
        }
        for (const QJsonValue &bv : msg.value(QStringLiteral("content")).toArray()) {
            const QJsonObject b = bv.toObject();
            const QString bt = b.value(QStringLiteral("type")).toString();
            if (bt == QLatin1String("tool_use")) {
                lastActivity = summarizeToolUse(
                    b.value(QStringLiteral("name")).toString(),
                    b.value(QStringLiteral("input")).toObject());
            } else if (bt == QLatin1String("text")) {
                const QString t = b.value(QStringLiteral("text")).toString().simplified();
                if (!t.isEmpty()) {
                    preview = t;
                }
            }
        }
    }
    f.close();
    return lastActivity;
}

QString WorkflowMonitor::fingerprint(const Snapshot &s)
{
    QString fp;
    fp += QString::number(int(s.state));
    fp += QLatin1Char('|') + QString::number(s.agentCount);
    fp += QLatin1Char('|') + QString::number(s.totalTokens);
    fp += QLatin1Char('|') + QString::number(s.totalToolCalls);
    fp += QLatin1Char('|') + QString::number(s.durationMs);
    fp += QLatin1Char('|') + s.summary;
    for (const Phase &p : s.phases) {
        fp += QStringLiteral("||") + p.title + QLatin1Char('#')
              + QString::number(p.agents.size());
        for (const SubAgent &a : p.agents) {
            fp += QLatin1Char('~') + a.agentId + QLatin1Char(':') + a.state
                  + QLatin1Char(':') + QString::number(a.tokens) + QLatin1Char(':')
                  + QString::number(a.toolCalls) + QLatin1Char(':') + a.lastActivity;
        }
    }
    fp += QStringLiteral("|logs:") + s.logs.join(QLatin1Char(';'));
    return fp;
}
