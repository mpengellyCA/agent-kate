// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

#include "state/Reactive.h"

// This translation unit exists so AUTOMOC processes Reactive.h's Q_OBJECT and
// the ReactiveBase vtable is emitted in exactly one object file (avoiding
// "undefined reference to vtable for ReactiveBase" at link time). The class
// template Reactive<T> stays header-only.

ReactiveBase::ReactiveBase(QObject *parent)
    : QObject(parent)
{
}

ReactiveBase::~ReactiveBase() = default;
