package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnvOverlaySecretsNeverReachDisk is the round trip the audit asked for
// (F2): a user who puts an API key in a per-thread env overlay must not find it
// in cleartext in threads.json — a file that outlives the thread, gets copied
// into the archive, and (before this change) was world-readable.
func TestEnvOverlaySecretsNeverReachDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const secret = "sk-live-do-not-persist-me"
	if err := s.Put(Record{
		ThreadID: "t-1",
		Project:  "/tmp/p",
		Env: map[string]string{
			"KIMI_API_KEY":   secret,
			"KIMI_CODE_HOME": "/home/u/.kimi-t-1",
		},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the API key was persisted in cleartext")
	}
	if !strings.Contains(string(raw), EnvNotStored) {
		t.Error("no redaction marker: the UI cannot tell a redacted overlay from an unset one")
	}
	// The non-secret half must survive verbatim — it is what points the CLI at
	// its per-thread state, and a resume without it looks in the wrong home.
	if !strings.Contains(string(raw), "/home/u/.kimi-t-1") {
		t.Error("KIMI_CODE_HOME was redacted; only credential-shaped keys should be")
	}

	// The IN-MEMORY record keeps the real value: this process can still
	// relaunch the thread it just started.
	live, ok := s.Get("t-1")
	if !ok {
		t.Fatal("record vanished")
	}
	if got := live.Env["KIMI_API_KEY"]; got != secret {
		t.Errorf("in-memory value = %q, want the real secret back", got)
	}
}

// TestEnvOverlayResolvedAtResume: on reopen the marker is re-resolved from the
// environment akcore itself runs in — the same mechanism the third-party
// provider token already uses (Record stores ProviderEnvVar, never the token).
func TestEnvOverlayResolvedAtResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Record{
		ThreadID: "t-1",
		Env:      map[string]string{"FIREWORKS_API_KEY": "written-once"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	t.Setenv("FIREWORKS_API_KEY", "resolved-at-resume")
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reopen): %v", err)
	}
	rec, ok := reopened.Get("t-1")
	if !ok {
		t.Fatal("record vanished")
	}
	if got := rec.Env["FIREWORKS_API_KEY"]; got != "resolved-at-resume" {
		t.Errorf("resolved value = %q, want the live environment's value", got)
	}
}

// TestEnvOverlayUnresolvedMarkerNeverReachesAChild: with nothing in the
// environment to resolve from, the marker survives (so the UI can still say
// "set, value not stored") but LaunchEnv drops it, so no child ever gets
// __agentkate_not_stored__ as a credential.
func TestEnvOverlayUnresolvedMarkerNeverReachesAChild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Record{
		ThreadID: "t-1",
		Env:      map[string]string{"SOME_TOKEN": "gone", "KIMI_CODE_HOME": "/home/u/.kimi"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	os.Unsetenv("SOME_TOKEN")

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reopen): %v", err)
	}
	rec, ok := reopened.Get("t-1")
	if !ok {
		t.Fatal("record vanished")
	}
	if got := rec.Env["SOME_TOKEN"]; got != EnvNotStored {
		t.Errorf("unresolved value = %q, want the marker preserved for the UI", got)
	}

	launch := LaunchEnv(rec.Env)
	if _, present := launch["SOME_TOKEN"]; present {
		t.Error("the marker would have been passed to the child as a credential")
	}
	if launch["KIMI_CODE_HOME"] != "/home/u/.kimi" {
		t.Error("LaunchEnv dropped a resolvable entry")
	}
}

// TestArchiveRedactsEnv: the archive is forever — a credential must not survive
// there after the live record is gone.
func TestArchiveRedactsEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const secret = "sk-archive-me-not"
	if err := s.Put(Record{ThreadID: "t-1", Env: map[string]string{"AWS_SECRET": secret}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Archive("t-1", "test"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "threads-archive.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the archive kept the credential in cleartext")
	}
	if !strings.Contains(string(raw), EnvNotStored) {
		t.Error("archive lost the redaction marker")
	}
}

// TestEnvKeyIsSecret pins the classifier, including the deliberate
// over-matching: a redacted non-secret costs one re-resolution, an
// un-redacted secret costs a cleartext credential in a file nobody deletes.
func TestEnvKeyIsSecret(t *testing.T) {
	secret := []string{
		"ANTHROPIC_API_KEY", "KIMI_API_KEY", "api_key", "GITHUB_TOKEN",
		"MY_SECRET", "DB_PASSWORD", "AWS_CREDENTIALS", "credential_file",
	}
	for _, k := range secret {
		if !EnvKeyIsSecret(k) {
			t.Errorf("%q classified as safe to persist", k)
		}
	}
	plain := []string{"KIMI_CODE_HOME", "PATH", "HTTP_PROXY", "LANG", "CLAUDE_CONFIG_DIR"}
	for _, k := range plain {
		if EnvKeyIsSecret(k) {
			t.Errorf("%q redacted; it carries no credential and a resume needs it", k)
		}
	}
}

// --- F2 second pass: the redaction net -------------------------------------

// The widened key heuristic must catch the credential-shaped names real
// providers use, and must NOT catch the ordinary variables whose names merely
// contain a fragment of one. Every false negative here is a cleartext secret in
// a file that is never deleted; every false positive is a user-set value that
// silently stops surviving a restart, so both directions are pinned.
func TestEnvKeyIsSecretCoversRealProviderNames(t *testing.T) {
	secret := []string{
		"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "KIMI_API_KEY",
		"OPENAI_APIKEY", "FIREWORKS_API_KEY", "OPENROUTER_API_KEY",
		"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_PAT", "githubPat",
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_ACCESS_KEY_ID",
		"DATABASE_PASSWORD", "PGPASSWORD", "MYSQL_PASSWD", "SSH_PASSPHRASE",
		"GPG_PRIVATE_KEY", "TLS_CERT_FILE", "CA_CERT", "SIGNING_SALT",
		"HTTP_AUTHORIZATION", "PROXY_AUTH", "X_AUTH", "WEBHOOK_SIGNATURE",
		"REFRESH_TOKEN", "REFRESH", "SESSION_COOKIE", "JWT_SECRET", "MY_JWT",
		"HMAC_SEED", "SENTRY_DSN", "APP_PW", "SUDO_PWD", "REQUEST_NONCE",
		"LOGIN_OTP", "SIG", "credential_store",
	}
	for _, k := range secret {
		if !EnvKeyIsSecret(k) {
			t.Errorf("EnvKeyIsSecret(%q) = false; a credential would reach disk", k)
		}
	}

	// The names that must survive: PATH is the one users genuinely override, and
	// redacting GIT_AUTHOR_* would silently change who commits appear to be from.
	innocent := []string{
		"PATH", "LD_LIBRARY_PATH", "PYTHONPATH", "COMPATIBILITY_MODE",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME",
		"KIMI_CODE_HOME", "HOME", "LANG", "TERM", "EDITOR", "TZ",
		"NO_COLOR", "AGENTKATE_THREAD_ID", "PASSTHROUGH_MODE", "SIGTERM_GRACE",
	}
	for _, k := range innocent {
		if EnvKeyIsSecret(k) {
			t.Errorf("EnvKeyIsSecret(%q) = true; an ordinary override would stop persisting", k)
		}
	}
}

// The value net catches what the key net structurally cannot: a credential in a
// variable whose name says nothing.
func TestEnvValueIsSecretCatchesObviousShapes(t *testing.T) {
	secret := []string{
		"sk-ant-api03-abcdef", "sk-proj-abcdef", "ghp_0123456789abcdef",
		"github_pat_11ABCDEF", "glpat-abcdefghijkl", "xoxb-1-2-abcdef",
		"AKIAIOSFODNN7EXAMPLE", "AIzaSyA-abcdefg", "ya29.a0AfH6",
		"hf_abcdefghijk", "Bearer abc.def.ghi", "bearer abc",
		"eyJhbGciOiJIUzI1NiJ9.e30.sig", "-----BEGIN OPENSSH PRIVATE KEY-----",
	}
	for _, v := range secret {
		if !EnvValueIsSecret(v) {
			t.Errorf("EnvValueIsSecret(%q) = false; a credential would reach disk", v)
		}
	}
	innocent := []string{
		"", "/home/u/.kimi-t-1", "true", "1", "https://api.example.com",
		"en_US.UTF-8", "sonnet", EnvNotStored, "skip", "skywalker",
	}
	for _, v := range innocent {
		if EnvValueIsSecret(v) {
			t.Errorf("EnvValueIsSecret(%q) = true; an ordinary value would stop persisting", v)
		}
	}
}

// End to end: a secret hiding behind an innocuous NAME must still not reach
// disk, because the value heuristic is wired into the persist path.
func TestEnvSecretValueUnderInnocentKeyNeverReachesDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "threads.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const secret = "sk-ant-api03-hiding-behind-a-boring-name"
	if err := s.Put(Record{
		ThreadID: "t-1", Project: "/tmp/p",
		Env: map[string]string{"MY_THING": secret, "TERM": "xterm-256color"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("a credential-shaped VALUE was persisted in cleartext under an innocent key")
	}
	if !strings.Contains(string(raw), "xterm-256color") {
		t.Error("an ordinary value was redacted")
	}
}

// --- F2 residual: the WIRE, not just the disk -------------------------------

// session.listThreads hands Store.List's result straight over the socket, and
// every agent bridge is a socket peer. A record's env overlay must be redacted
// there too — otherwise one prompt-injected agent reads every other thread's
// credentials without touching a file.
func TestListRedactsEnvOverTheWire(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const secret = "sk-live-not-for-other-agents"
	if err := s.Put(Record{
		ThreadID: "t-1", Project: "/tmp/p",
		Env: map[string]string{"KIMI_API_KEY": secret, "KIMI_CODE_HOME": "/home/u/.k"},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	list := s.List("")
	if len(list) != 1 {
		t.Fatalf("List returned %d records, want 1", len(list))
	}
	if got := list[0].Env["KIMI_API_KEY"]; got != EnvNotStored {
		t.Errorf("List leaked the credential over the wire: %q", got)
	}
	if got := list[0].Env["KIMI_CODE_HOME"]; got != "/home/u/.k" {
		t.Errorf("List redacted an ordinary value: %q", got)
	}
	// Redaction must be a COPY: the in-memory record still has to be able to
	// relaunch the thread.
	live, _ := s.Get("t-1")
	if live.Env["KIMI_API_KEY"] != secret {
		t.Fatal("List mutated the live record; the thread can no longer relaunch")
	}
}

// ListArchived is the worse half: loadArchive re-resolves markers from akcore's
// OWN environment so a Restore relaunches with a live credential — values that
// were never on disk at all. Handing those to a socket peer would be a leak the
// disk redaction cannot see.
func TestListArchivedRedactsEnvOverTheWire(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const daemonSecret = "sk-live-from-the-daemon-environment"
	t.Setenv("AGENTKATE_TEST_API_KEY", daemonSecret)
	if err := s.Put(Record{
		ThreadID: "t-1", Project: "/tmp/p",
		Env: map[string]string{"AGENTKATE_TEST_API_KEY": daemonSecret},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Archive("t-1", "test"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	arch := s.ListArchived()
	if len(arch) != 1 {
		t.Fatalf("ListArchived returned %d records, want 1", len(arch))
	}
	if got := arch[0].Env["AGENTKATE_TEST_API_KEY"]; got != EnvNotStored {
		t.Errorf("ListArchived leaked the daemon's credential over the wire: %q", got)
	}
	// Restore still gets the resolved value: redaction is on the listing, not on
	// the operational path.
	if err := s.Restore("t-1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	live, ok := s.Get("t-1")
	if !ok {
		t.Fatal("restored record missing")
	}
	if live.Env["AGENTKATE_TEST_API_KEY"] != daemonSecret {
		t.Errorf("Restore lost the resolved credential: %q",
			live.Env["AGENTKATE_TEST_API_KEY"])
	}
}
