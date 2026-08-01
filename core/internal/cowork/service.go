package cowork

import (
	"log/slog"
	"time"

	"agentkate/internal/kde"
	"agentkate/internal/safe"
)

// Service is the single object handlerDeps holds. It embeds the consent Authority
// and owns the shared kde.Client. Introspection state (window cache, sandbox) is
// added in v2 (plan 07 §2, C5).
type Service struct {
	*Authority
	kde    *kde.Client
	portal *kde.PortalBroker
	log    *slog.Logger

	sweepStop chan struct{}
}

// New loads the grant store and audit log, builds the Authority, and wires the
// store's change hook to a grantsChanged notification. The returned warnings are
// non-fatal startup conditions worth surfacing (corrupt grants file, tampered
// audit) — the service still starts (fail-closed where it matters: a tampered
// audit makes Authorize deny everything).
//
// kdeClient may be nil (e.g. no session bus); capability calls that need it return
// a clean "unavailable" error rather than panicking.
func New(grantsPath, auditPath, policyPath string, kdeClient *kde.Client, notify Notifier, log *slog.Logger) (*Service, []string, error) {
	if log == nil {
		log = slog.Default()
	}
	var warnings []string

	store, err := LoadStore(grantsPath)
	if err != nil {
		warnings = append(warnings, err.Error())
		log.Warn("cowork: grant store load issue", "err", err)
	}
	audit, err := LoadAudit(auditPath)
	if err != nil {
		return nil, warnings, err
	}
	if audit.Tampered() {
		warnings = append(warnings, "consent audit chain failed verification — desktop access is disabled until reset")
		log.Error("cowork: audit chain tampered; access fails closed")
	}
	policy, err := LoadPolicy(policyPath)
	if err != nil {
		warnings = append(warnings, err.Error())
		log.Warn("cowork: policy load issue", "err", err)
		policy, _ = LoadPolicy("") // empty deny-all fallback
	}

	auth := newAuthority(store, audit, policy, notify, log)
	store.SetOnChange(func() {
		if notify != nil {
			notify.NotifyUI("cowork.grantsChanged", map[string]any{})
		}
	})
	policy.SetOnChange(func() {
		if notify != nil {
			notify.NotifyUI("cowork.policyChanged", map[string]any{})
		}
	})

	s := &Service{Authority: auth, kde: kdeClient, portal: kde.NewPortalBroker(), log: log, sweepStop: make(chan struct{})}
	return s, warnings, nil
}

// KDE returns the shared kde client (may be nil if the session bus was unavailable).
func (s *Service) KDE() *kde.Client { return s.kde }

// Portal returns the core↔UI portal round-trip broker.
func (s *Service) Portal() *kde.PortalBroker { return s.portal }

// Available reports whether the KDE session bus is reachable (capability probe).
func (s *Service) Available() bool { return s.kde != nil }

// StartSweeper periodically revokes expired grants. Launch once after construction.
func (s *Service) StartSweeper(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	safe.Go("cowork.sweepExpired", func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.sweepStop:
				return
			case <-t.C:
				s.Authority.store.SweepExpired()
			}
		}
	})
}

// Close stops background work, tears down live sessions, and closes the kde client.
func (s *Service) Close() error {
	select {
	case <-s.sweepStop:
		// already closed
	default:
		close(s.sweepStop)
	}
	// Graceful teardown of any live portal/screencast sessions. This is NOT the panic
	// Kill: a normal quit must leave the user's standing policy toggles intact (they are
	// meant to persist across restarts) and must not spam the audit chain with a "kill"
	// entry on every exit. Non-durable grants lapse on next load (restart semantics).
	s.Authority.Shutdown()
	if s.kde != nil {
		return s.kde.Close()
	}
	return nil
}
