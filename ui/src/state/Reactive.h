// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#pragma once

#include <QObject>

#include <functional>
#include <utility>

// ---------------------------------------------------------------------------
// Reactive<T> — a tiny "Vue-like" reactive primitive.
//
// Purpose: let the UI emit/repaint ONLY when state actually changed. Several
// models (git tree, worktree dashboard, ...) are fed identical data on every
// poll; re-emitting unconditionally causes visible flicker. Wrapping the
// model's backing state in a Reactive<T> makes redundant updates SILENT.
//
// Contract / invariant:
//   set(next) performs a STRUCTURAL DIFF. If `next == m_value`, it is a no-op:
//   no assignment, no `changed()` signal, no subscriber callbacks. Only a
//   genuinely different value assigns and emits exactly once. That single
//   property is the whole point — keep it trivially correct.
//
// Requirements on T:
//   - copyable / movable
//   - equality-comparable: `bool operator==(const T&, const T&)` must exist.
//     (Most Qt value types — QString, QVector<T>, QHash<...>, and aggregates
//      whose members are all comparable — satisfy this.)
//
// MOC note: Qt signals require Q_OBJECT, but a class template cannot be a
// QObject (MOC cannot moc a template). So the signal lives on the non-template
// base ReactiveBase, and Reactive<T> *contains* one. ReactiveBase is declared
// here and defined in Reactive.cpp so AUTOMOC emits its vtable exactly once.
// ReactiveBase deliberately has NO template members.
// ---------------------------------------------------------------------------

class ReactiveBase : public QObject
{
    Q_OBJECT
public:
    explicit ReactiveBase(QObject *parent = nullptr);
    ~ReactiveBase() override;

Q_SIGNALS:
    // Emitted exactly once whenever the owning Reactive<T> assigns a value that
    // differs (by operator==) from the previous one.
    void changed();
};

template <class T>
class Reactive
{
public:
    Reactive() = default;
    explicit Reactive(T initial) : m_value(std::move(initial)) {}

    // Reactive owns a ReactiveBase; it is neither copyable nor movable so that
    // subscribers' connections to the base remain valid for its lifetime.
    Reactive(const Reactive &) = delete;
    Reactive &operator=(const Reactive &) = delete;
    Reactive(Reactive &&) = delete;
    Reactive &operator=(Reactive &&) = delete;

    // Current canonical value.
    const T &get() const { return m_value; }

    // Structural diff: silently ignore equal assignments; otherwise assign and
    // emit changed() exactly once.
    void set(T next)
    {
        if (next == m_value) {
            return;
        }
        m_value = std::move(next);
        Q_EMIT m_base.changed();
    }

    // Subscribe to changes. `fn` runs whenever the value actually changes; the
    // connection is bound to `ctx`'s lifetime (it is auto-disconnected when
    // `ctx` is destroyed), so subscribers never fire after they are gone.
    void subscribe(QObject *ctx, std::function<void(const T &)> fn)
    {
        QObject::connect(&m_base, &ReactiveBase::changed, ctx,
                         [this, fn = std::move(fn)]() { fn(m_value); });
    }

    // Direct access to the underlying signal source, e.g. to connect()
    // a slot manually or relay `changed()` onward.
    ReactiveBase *notifier() { return &m_base; }

private:
    T m_value{};
    ReactiveBase m_base;
};
