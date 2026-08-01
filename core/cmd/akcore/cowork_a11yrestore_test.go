package main

import (
	"path/filepath"
	"testing"

	"agentkate/internal/session"
)

// --- F8: turning desktop access off must actually restore the desktop flags -----------
//
// The enable dialog and the per-action control prompt both promise the desktop-wide
// org.a11y.Status flip lasts only until desktop access is turned off. Until this gate
// existed, only the kill-switch and app exit honoured that — switching the last agent
// off left every application on the session exporting its accessibility tree while the
// UI said otherwise. noCoworkThreadsLeft is the decision behind the restore
// notification; it must fail SAFE (no restore) rather than restore under a live agent.

func a11yTestStore(t *testing.T) *session.Store {
	t.Helper()
	st, err := session.NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	return st
}

func putThread(t *testing.T, st *session.Store, id string, cowork bool) {
	t.Helper()
	if err := st.Put(session.Record{ThreadID: id, Project: "/p", CoworkEnabled: cowork}); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
}

func TestRestoreDesktopFlagsOnlyWhenNoCoworkThreadRemains(t *testing.T) {
	st := a11yTestStore(t)
	d := handlerDeps{sessions: st}

	putThread(t, st, "t-a", true)
	putThread(t, st, "t-b", true)
	if noCoworkThreadsLeft(d) {
		t.Fatal("two agents still hold desktop access — the flags must stay on")
	}

	// One off, one still on: restoring here would break the live agent's element reads.
	if err := st.Update("t-a", func(r *session.Record) { r.CoworkEnabled = false }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if noCoworkThreadsLeft(d) {
		t.Fatal("one agent still holds desktop access — the flags must stay on")
	}

	// The last one off: this is the moment the dialog's promise comes due.
	if err := st.Update("t-b", func(r *session.Record) { r.CoworkEnabled = false }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !noCoworkThreadsLeft(d) {
		t.Fatal("no agent holds desktop access — the accessibility flip must be restored")
	}
}

func TestRestoreDesktopFlagsHeldBackWhenTheStoreIsUnreadable(t *testing.T) {
	// No store: we cannot prove nobody is relying on the flip, and restoring it under a
	// live agent silently breaks reads. Fail safe by leaving it alone.
	if noCoworkThreadsLeft(handlerDeps{}) {
		t.Fatal("an unreadable session store must not be read as 'nobody is using it'")
	}
}
