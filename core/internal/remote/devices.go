package remote

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentkate/internal/fsperm"
	"golang.org/x/sys/unix"
)

// tokenBytes is the size of a device pairing token. 256 bits of crypto/rand is
// far beyond brute force over a rate-limited LAN endpoint, which is precisely
// why the store needs no slow KDF: a token is not a password, there is no
// low-entropy secret to stretch. A plain SHA-256 is the right primitive for a
// high-entropy credential, and it means nothing recoverable is ever at rest.
const tokenBytes = 32

// tokenChars is the exact length of a base64url-nopad encoding of tokenBytes.
// Checking it before hashing means a caller cannot make us hash a megabyte.
const tokenChars = 43

// sessionTTL matches the Max-Age in the frozen contract (2592000 seconds).
// A month is long enough that a phone that only checks in at weekends stays
// paired, and short enough that a forgotten device eventually falls out.
const sessionTTL = 30 * 24 * time.Hour

// deviceStoreSchemaVersion guards against loading a file written by a newer
// core. Following cowork.LoadStore, a newer schema refuses to load rather than
// silently inheriting state it does not understand.
const deviceStoreSchemaVersion = 2

// cookieName is the session cookie. It is set Secure; HttpOnly; SameSite=Strict;
// Path=/ — see setSessionCookie.
const cookieName = "ak_remote"

// cookieVersion prefixes every session cookie so the format can change without
// a stale cookie being misparsed as a valid one.
const cookieVersion = "v1"

var errBadToken = errors.New("remote: bad token")

// Device is one paired phone.
//
// TokenHash is the ONLY representation of the pairing token that survives the
// mint call — there is no plaintext at rest, so a reader of this file (including
// an agent running at the same uid) learns nothing it can present to the server.
// Epoch is the revocation lever for session cookies: cookies are stateless HMACs
// that must survive a core restart (the core dies whenever the desktop closes),
// so they cannot be invalidated by forgetting them. Bumping Epoch invalidates
// every cookie this device holds, atomically and durably.
type Device struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	TokenHash    string     `json:"tokenHash"`
	PairedAt     time.Time  `json:"pairedAt"`
	LastSeen     time.Time  `json:"lastSeen,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	RevokeReason string     `json:"revokeReason,omitempty"`
	Epoch        int        `json:"epoch"`
	// Capabilities are explicit opt-ins for a trusted developer device.  A
	// normal paired phone has none.  Keeping them on the device rather than in
	// a browser claim means the desktop can revoke a privilege centrally.
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// Capability is one separately granted remote-developer power.  These are not
// Cowork capabilities: none grant desktop capture, input, or UI authority.
type Capability string

const (
	CapAgentManage    Capability = "agent_manage"
	CapAgentConfigure Capability = "agent_configure"
	CapWorktreeView   Capability = "worktree_view"
	CapWorktreeEdit   Capability = "worktree_edit"
)

func (c Capability) Valid() bool {
	switch c {
	case CapAgentManage, CapAgentConfigure, CapWorktreeView, CapWorktreeEdit:
		return true
	default:
		return false
	}
}

// Active reports whether the device may still authenticate.
func (d *Device) Active() bool { return d != nil && d.RevokedAt == nil }

func (d *Device) clone() *Device {
	c := *d
	c.Capabilities = append([]Capability(nil), d.Capabilities...)
	if d.RevokedAt != nil {
		t := *d.RevokedAt
		c.RevokedAt = &t
	}
	return &c
}

func (d *Device) Allows(cap Capability) bool {
	if d == nil || !d.Active() || !cap.Valid() {
		return false
	}
	for _, current := range d.Capabilities {
		if current == cap {
			return true
		}
	}
	return false
}

type deviceFile struct {
	SchemaVersion int `json:"schemaVersion"`
	// SessionSecret keys the cookie HMAC. It lives beside the device hashes
	// because both are equally sensitive and both must survive a restart.
	SessionSecret string    `json:"sessionSecret"`
	KillSwitch    bool      `json:"killSwitch"`
	Devices       []*Device `json:"devices"`
}

// DeviceStore is the durable paired-device table, modelled on cowork.Store:
// mutex-guarded, copy-out, and flock-serialised across processes so two akcore
// instances sharing a data dir cannot lose each other's writes.
type DeviceStore struct {
	mu         sync.Mutex
	path       string
	secret     []byte
	killSwitch bool
	devices    map[string]*Device
	onChange   func()
	now        func() time.Time
}

// LoadDeviceStore reads the device file without creating one. Merely opening a
// Remote Access panel must not mint credentials or leave a durable footprint;
// the first device mutation creates the table and its session secret.
func LoadDeviceStore(path string, now func() time.Time) (*DeviceStore, error) {
	if now == nil {
		now = time.Now
	}
	s := &DeviceStore{path: path, devices: map[string]*Device{}, now: now}
	if _, err := fsperm.HardenFile(path); err != nil {
		return nil, fmt.Errorf("remote: devices permissions: %w", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("remote: read devices: %w", err)
	}
	var fm deviceFile
	if err := json.Unmarshal(b, &fm); err != nil {
		return nil, fmt.Errorf("remote: devices file is corrupt; refusing to replace credentials: %w", err)
	}
	if fm.SchemaVersion > deviceStoreSchemaVersion {
		return s, fmt.Errorf("remote: devices schema v%d newer than supported v%d; refusing to load (no devices paired)",
			fm.SchemaVersion, deviceStoreSchemaVersion)
	}
	s.killSwitch = fm.KillSwitch
	for _, d := range fm.Devices {
		if d == nil || d.ID == "" || d.TokenHash == "" {
			continue
		}
		s.devices[d.ID] = d
	}
	secret, derr := hex.DecodeString(fm.SessionSecret)
	if derr != nil || len(secret) != tokenBytes {
		// Fail closed without overwriting the file at read time. A later explicit
		// pairing mutation mints a fresh secret and invalidates old cookies.
		return s, nil
	}
	s.secret = secret
	return s, nil
}

// SetCapabilities replaces one device's trusted-developer grants.  The
// desktop is the only caller.  Unknown values are rejected rather than stored
// for a future route to accidentally honour.
func (s *DeviceStore) SetCapabilities(id string, caps []Capability) (Device, bool, error) {
	unique := make(map[Capability]struct{}, len(caps))
	for _, cap := range caps {
		if !cap.Valid() {
			return Device{}, false, fmt.Errorf("remote: unknown capability %q", cap)
		}
		unique[cap] = struct{}{}
	}
	next := make([]Capability, 0, len(unique))
	for _, cap := range []Capability{CapAgentManage, CapAgentConfigure, CapWorktreeView, CapWorktreeEdit} {
		if _, ok := unique[cap]; ok {
			next = append(next, cap)
		}
	}
	s.mu.Lock()
	d, ok := s.devices[id]
	if !ok || !d.Active() {
		s.mu.Unlock()
		return Device{}, false, nil
	}
	changed := !sameCapabilities(d.Capabilities, next)
	if changed {
		d.Capabilities = next
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return Device{}, false, err
		}
	}
	out := *d.clone()
	s.mu.Unlock()
	if changed {
		s.fireOnChange()
	}
	return out, changed, nil
}

func sameCapabilities(a, b []Capability) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *DeviceStore) ensureSecretLocked() error {
	if len(s.secret) == tokenBytes {
		return nil
	}
	secret := make([]byte, tokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("remote: session secret: %w", err)
	}
	s.secret = secret
	return nil
}

// SetOnChange registers a callback fired after every mutation, outside the lock,
// so the UI can be told the paired-device list moved (cowork.Store.SetOnChange).
func (s *DeviceStore) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *DeviceStore) lockPath() string { return s.path + ".lock" }

// withFileLock serialises read-modify-write across concurrent core instances.
func (s *DeviceStore) withFileLock(fn func() error) error {
	f, err := openPrivate(s.lockPath(), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("remote: open lock: %w", err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("remote: flock: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// saveLocked writes the file atomically at 0600. Caller holds s.mu.
func (s *DeviceStore) saveLocked() error {
	return s.withFileLock(func() error {
		ds := make([]*Device, 0, len(s.devices))
		for _, d := range s.devices {
			ds = append(ds, d)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].PairedAt.Before(ds[j].PairedAt) })
		b, err := json.MarshalIndent(deviceFile{
			SchemaVersion: deviceStoreSchemaVersion,
			SessionSecret: hex.EncodeToString(s.secret),
			KillSwitch:    s.killSwitch,
			Devices:       ds,
		}, "", "  ")
		if err != nil {
			return err
		}
		return writePrivateAtomic(s.path, b)
	})
}

func (s *DeviceStore) fireOnChange() {
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// HashToken is the one definition of "how a token becomes a stored hash", so the
// mint path and the verify path cannot drift.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Mint creates a device and returns its pairing token in plaintext exactly once.
// The caller must hand it to the user immediately (B4's shareUrl); it is
// unrecoverable afterwards.
func (s *DeviceStore) Mint(name string) (token string, dev Device, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Device{}, fmt.Errorf("remote: mint token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	idRaw := make([]byte, 8)
	if _, err := rand.Read(idRaw); err != nil {
		return "", Device{}, fmt.Errorf("remote: mint id: %w", err)
	}
	d := &Device{
		ID:        "d-" + hex.EncodeToString(idRaw),
		Name:      strings.TrimSpace(name),
		TokenHash: HashToken(token),
		PairedAt:  s.now().UTC(),
	}
	if d.Name == "" {
		d.Name = "phone"
	}
	s.mu.Lock()
	if err := s.ensureSecretLocked(); err != nil {
		s.mu.Unlock()
		return "", Device{}, err
	}
	s.devices[d.ID] = d
	err = s.saveLocked()
	out := d.clone()
	s.mu.Unlock()
	s.fireOnChange()
	return token, *out, err
}

// Verify exchanges a plaintext token for its device.
//
// The lookup walks every device with a constant-time comparison rather than
// indexing a map by hash. A map probe on a SHA-256 digest leaks nothing in
// practice, but the walk costs microseconds for a handful of phones and removes
// the need to reason about it at all.
func (s *DeviceStore) Verify(token string) (Device, error) {
	if len(token) != tokenChars {
		return Device{}, errBadToken
	}
	want := HashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killSwitch || len(s.secret) != tokenBytes {
		return Device{}, errBadToken
	}
	var found *Device
	for _, d := range s.devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(want)) == 1 && d.Active() {
			found = d
		}
	}
	if found == nil {
		return Device{}, errBadToken
	}
	found.LastSeen = s.now().UTC()
	_ = s.saveLocked()
	return *found.clone(), nil
}

// Get returns a device copy by id.
func (s *DeviceStore) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, false
	}
	return *d.clone(), true
}

// List returns every device (revoked ones included, so the panel can show
// history), newest first.
func (s *DeviceStore) List() []Device {
	s.mu.Lock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, *d.clone())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].PairedAt.After(out[j].PairedAt) })
	return out
}

// Revoke marks a device revoked and bumps its epoch, which invalidates every
// session cookie it holds. It reports whether anything changed.
//
// Revoking the credential is only half the job: an SSE stream opened before the
// revoke would keep feeding a revoked device indefinitely. Server.RevokeDevice
// pairs this with terminating those streams.
func (s *DeviceStore) Revoke(id, reason string) bool {
	s.mu.Lock()
	d, ok := s.devices[id]
	changed := false
	if ok && d.RevokedAt == nil {
		t := s.now().UTC()
		d.RevokedAt = &t
		d.RevokeReason = reason
		d.Epoch++
		_ = s.saveLocked()
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.fireOnChange()
	}
	return changed
}

// SetKillSwitch engages or re-arms the global switch. While engaged no token
// exchanges and no cookie verifies, so the surface is closed without tearing
// down the listener — a phone gets a clear "disabled" answer rather than a
// connection refused it cannot distinguish from being out of range.
func (s *DeviceStore) SetKillSwitch(on bool) {
	s.mu.Lock()
	changed := s.killSwitch != on
	s.killSwitch = on
	if changed {
		if s.ensureSecretLocked() == nil {
			_ = s.saveLocked()
		}
	}
	s.mu.Unlock()
	if changed {
		s.fireOnChange()
	}
}

// KillSwitch reports whether the global switch is engaged.
func (s *DeviceStore) KillSwitch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killSwitch
}

// --- session cookies -------------------------------------------------------
//
// The cookie is a stateless, HMAC-authenticated bearer string rather than a key
// into an in-memory session table. That is forced by a real constraint: the core
// exits when the last IPC client disconnects (it is not a daemon), so a
// memory-resident session dies every time the desktop app closes — and the phone
// cannot re-exchange, because the pairing token was deliberately erased from its
// address bar the moment it was used. A restart-surviving cookie is therefore
// the difference between "remote access" and "remote access until you close the
// app once".
//
// Format: v1.<deviceId>.<expiryUnix>.<nonce>.<epoch>.<base64url mac>
// mac = HMAC-SHA256(secret, "v1.<deviceId>.<expiry>.<nonce>.<epoch>")
//
// The epoch is carried in the cookie AND compared against the device record at
// verify time. That comparison is what makes a revoke invalidate outstanding
// cookies instantly, even though the server keeps no session state: the record
// moves, the cookie cannot.

// Session is a verified cookie.
type Session struct {
	DeviceID   string
	DeviceName string
	// ID is the cookie's nonce. It identifies this one browser session, so a
	// logout can drop exactly its own live SSE stream and nobody else's.
	ID      string
	Expires time.Time
}

// NewSession mints a cookie value for a device.
func (s *DeviceStore) NewSession(deviceID string) (value string, expires time.Time, err error) {
	nonceRaw := make([]byte, 16)
	if _, err := rand.Read(nonceRaw); err != nil {
		return "", time.Time{}, fmt.Errorf("remote: session nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceRaw)
	expires = s.now().UTC().Add(sessionTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[deviceID]
	if !ok || !d.Active() || len(s.secret) != tokenBytes {
		return "", time.Time{}, errBadToken
	}
	body := sessionBody(deviceID, expires.Unix(), nonce, d.Epoch)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return body + "." + sig, expires, nil
}

func sessionBody(deviceID string, exp int64, nonce string, epoch int) string {
	return cookieVersion + "." + deviceID + "." + strconv.FormatInt(exp, 10) + "." + nonce +
		"." + strconv.Itoa(epoch)
}

// VerifySession authenticates a cookie value. It fails closed on every error:
// bad format, bad signature, expiry, an unknown or revoked device, a bumped
// epoch, or the global kill-switch.
func (s *DeviceStore) VerifySession(value string) (Session, bool) {
	parts := strings.Split(value, ".")
	// version, deviceId, expiry, nonce, epoch, mac
	if len(parts) != 6 || parts[0] != cookieVersion {
		return Session{}, false
	}
	deviceID, expStr, nonce, epochStr, sig := parts[1], parts[2], parts[3], parts[4], parts[5]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return Session{}, false
	}
	epoch, err := strconv.Atoi(epochStr)
	if err != nil {
		return Session{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killSwitch || len(s.secret) != tokenBytes {
		return Session{}, false
	}
	d, ok := s.devices[deviceID]
	if !ok || !d.Active() || d.Epoch != epoch {
		return Session{}, false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(sessionBody(deviceID, exp, nonce, epoch)))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// Constant time: the signature is the only thing standing between an
	// unauthenticated caller and every agent on this machine.
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return Session{}, false
	}
	if s.now().After(time.Unix(exp, 0)) {
		return Session{}, false
	}
	return Session{
		DeviceID:   deviceID,
		DeviceName: d.Name,
		ID:         nonce,
		Expires:    time.Unix(exp, 0).UTC(),
	}, true
}

// dataFile mirrors cowork.dataFile — the data-dir convention copy-pasted across
// the core. Kept identical rather than factored out because every other package
// has its own copy and one divergent helper would be worse than five identical
// ones.
func dataFile(name string) string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", name)
}

// DefaultDevicesPath is where paired-device hashes live.
func DefaultDevicesPath() string { return dataFile("remote-devices.json") }

// DefaultAuditPath is where the remote action audit chain lives.
func DefaultAuditPath() string { return dataFile("remote-audit.jsonl") }
