package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certValidity is how long a generated certificate lives.
//
// 397 days is the maximum a modern browser will accept for a server certificate
// (the CA/Browser Forum limit, enforced by Safari and followed by Chrome). It
// applies to publicly-trusted chains rather than a manually-accepted self-signed
// cert, but choosing a number no browser can object to costs nothing and removes
// a class of "it works on my desktop browser" surprise on a phone.
const certValidity = 397 * 24 * time.Hour

// certRenewWindow forces regeneration before expiry, so a certificate never
// expires while somebody is relying on it and has to be re-accepted at the worst
// possible moment.
const certRenewWindow = 30 * 24 * time.Hour

const (
	certFileName = "remote-cert.pem"
	keyFileName  = "remote-key.pem"
)

// ensureCert loads the stored certificate, regenerating it when it is missing,
// expiring, or does not cover bindHost.
//
// SAN coverage is the part that bites. A mobile browser rejects a certificate
// whose subjectAltName does not contain the exact host in the address bar — and
// the address bar holds an IP, because a LAN has no name for this machine. A
// certificate generated for one network is therefore worthless on the next one,
// so the bind address is checked on every start rather than only at first
// enable.
func ensureCert(dir, bindHost string, now time.Time) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	hosts := certHosts(bindHost)
	if cert, fp, ok := loadUsableCert(certPath, keyPath, hosts, now); ok {
		return cert, fp, nil
	}
	return generateCert(dir, certPath, keyPath, hosts, now)
}

// certHosts is the SAN set: the chosen bind address plus loopback, so the same
// certificate serves a phone on the LAN and a browser on this machine.
func certHosts(bindHost string) []string {
	out := []string{"localhost", "127.0.0.1", "::1"}
	if h := strings.Trim(strings.TrimSpace(bindHost), "[]"); h != "" && !isWildcardHost(h) {
		out = append(out, h)
	} else {
		// A wildcard bind (explicitly opted into) has no single address to
		// promise, so every routable local address goes in — otherwise the one
		// the user actually types is the one that fails.
		out = append(out, localAddresses()...)
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		out = append(out, name)
	}
	return dedupe(out)
}

func localAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			out = append(out, ipNet.IP.String())
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func loadUsableCert(certPath, keyPath string, hosts []string, now time.Time) (tls.Certificate, string, bool) {
	certPEM, err := readPrivate(certPath)
	if err != nil {
		return tls.Certificate{}, "", false
	}
	keyPEM, err := readPrivate(keyPath)
	if err != nil {
		return tls.Certificate{}, "", false
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", false
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, "", false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", false
	}
	if now.Add(certRenewWindow).After(leaf.NotAfter) {
		return tls.Certificate{}, "", false
	}
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return tls.Certificate{}, "", false
		}
	}
	cert.Leaf = leaf
	return cert, fingerprint(cert.Certificate[0]), true
}

func generateCert(dir, certPath, keyPath string, hosts []string, now time.Time) (tls.Certificate, string, error) {
	// P-256 rather than RSA: universally supported by mobile browsers, and a
	// keygen that takes microseconds rather than seconds matters when the first
	// thing "enable remote access" does is block on it.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: generate key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Agent Kate remote access", Organization: []string{"Agent Kate"}},
		// Backdated a minute so a phone whose clock runs slightly behind does not
		// reject a certificate that was valid the moment it was made.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("remote: marshal key: %w", err)
	}

	if err := ensurePrivateDir(dir); err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writePrivateAtomic(certPath, certPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	// 0600 on the key is the minimum; it buys nothing against a same-uid agent
	// (the honest posture recorded throughout the Cowork work) but it does keep
	// the key away from every other user on a shared host.
	if err := writePrivateAtomic(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, "", err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert.Leaf = leaf
	return cert, fingerprint(der), nil
}

// fingerprint renders a certificate's SHA-256 the way every browser's
// certificate viewer does, so the two strings can be compared by eye. That
// comparison is the only real defence self-signed TLS has against an active
// MITM, because the warning dialog itself has trained everybody to tap through.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}
