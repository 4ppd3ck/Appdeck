// Free TLS for the LAN dashboard: a self-signed certificate generated on
// first boot (or on demand) and served on a second, HTTPS-only port. The
// plain-HTTP listener stays untouched — the native window and loopback
// browsing keep using it, since loopback traffic never crosses the network,
// while remote/Wi-Fi browsers switch to https://<lan-ip>:<tls-port>.
//
// The certificate costs nothing, needs no CA, and never expires the
// launcher: it lives in <data>/tls/{cert,key}.pem and is regenerated with a
// fresh private key by POST /api/tls/regenerate (loopback only) — the
// "I think I'm compromised" button. Rotation is also automatic: on boot and
// once a day, a certificate inside the rotation window
// (LAUNCHER_TLS_ROTATE_DAYS, default 30 days before expiry) is regenerated
// with a new private key and swapped in live. A browser that clicked through
// the old certificate warns again on the new one; deleting <data>/tls and
// restarting has the same effect.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// tlsCertDir returns <data>/tls — where the generated pair lives.
func tlsCertDir(dataDir string) string {
	return filepath.Join(dataDir, "tls")
}

// tlsPortFromEnv resolves the HTTPS port: LAUNCHER_TLS=0 disables it,
// LAUNCHER_TLS_PORT overrides the default 9443.
func tlsPortFromEnv() int {
	if os.Getenv("LAUNCHER_TLS") == "0" {
		return 0
	}
	p := envInt("LAUNCHER_TLS_PORT", 9443)
	if p < 0 || p > 65535 {
		return 0
	}
	return p
}

// certProvider hands the current certificate to the TLS listener through
// GetCertificate, so a regeneration swaps it in live — no restart. The parsed
// leaf rides along for the rotation check.
type certProvider struct {
	cert atomic.Pointer[tls.Certificate]
	leaf atomic.Pointer[x509.Certificate]
}

func (p *certProvider) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := p.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("no TLS certificate loaded")
	}
	return c, nil
}

// loadLeaf returns the live certificate's parsed leaf (nil when unset or on
// a nil provider — a nil lock-style guard for test-built servers).
func (p *certProvider) loadLeaf() *x509.Certificate {
	if p == nil {
		return nil
	}
	return p.leaf.Load()
}

// tlsRotateWindow is how close to expiry a certificate must be before it is
// rotated: LAUNCHER_TLS_ROTATE_DAYS, default 30. The 397-day lifetime means
// a launcher left running (or never restarted) still rotates a year out.
func tlsRotateWindow() time.Duration {
	days := envInt("LAUNCHER_TLS_ROTATE_DAYS", 30)
	if days < 0 {
		days = 30
	}
	return time.Duration(days) * 24 * time.Hour
}

// certExpiringSoon reports whether the leaf is missing or expires within the
// rotation window.
func certExpiringSoon(leaf *x509.Certificate, window time.Duration) bool {
	return leaf == nil || time.Until(leaf.NotAfter) < window
}

// localCertSANs lists the names the certificate answers to: the machine's
// hostname, loopback, and every non-loopback interface IP, so browsers that
// connect by LAN address match the SAN (the self-signed warning remains —
// trusting it is a click-through, see SECURITY.md).
func localCertSANs() (dns []string, ips []net.IP) {
	if host, err := os.Hostname(); err == nil && host != "" {
		dns = append(dns, host)
	}
	ips = append(ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	ifaces, err := net.Interfaces()
	if err != nil {
		return dns, ips
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, ip4)
		}
	}
	return dns, ips
}

// generateServerCert creates a fresh self-signed pair (new private key) with
// the standard 397-day lifetime — the longest span CA/Browser forum rules
// allow for public certs, and plenty for a self-signed one that rotates.
func generateServerCert() (certPEM, keyPEM []byte, leaf *x509.Certificate, err error) {
	return generateServerCertWith(397 * 24 * time.Hour)
}

// generateServerCertWith is the generator with a caller-chosen lifetime —
// the knob the rotation tests use to stage near-expiry pairs.
func generateServerCertWith(lifetime time.Duration) (certPEM, keyPEM []byte, leaf *x509.Certificate, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, nil, err
	}
	dns, ips := localCertSANs()
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AppDeck", Organization: []string{"AppDeck"}},
		NotBefore:             now.Add(-time.Hour), // tolerate clock skew
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	return certPEM, keyPEM, leaf, nil
}

// writeTLSPair stores the pair atomically (tmp + rename each).
func writeTLSPair(dataDir string, certPEM, keyPEM []byte) error {
	dir := tlsCertDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, data := range map[string][]byte{certPath: certPEM, keyPath: keyPEM} {
		mode := os.FileMode(0o644)
		if path == keyPath {
			mode = 0o600 // the private key is the pair's secret half
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// ensureServerCert loads <data>/tls, generating the pair on first boot — and
// rotating it when the existing certificate sits inside the rotation window,
// so an install that rarely restarts still never serves a near-expiry cert.
func ensureServerCert(dataDir string) (*certProvider, error) {
	certPath := filepath.Join(tlsCertDir(dataDir), "cert.pem")
	keyPath := filepath.Join(tlsCertDir(dataDir), "key.pem")
	mkPair := func() (*tls.Certificate, *x509.Certificate, error) {
		certPEM, keyPEM, leaf, err := generateServerCert()
		if err != nil {
			return nil, nil, fmt.Errorf("generating the TLS certificate: %w", err)
		}
		if err := writeTLSPair(dataDir, certPEM, keyPEM); err != nil {
			return nil, nil, fmt.Errorf("writing the TLS certificate: %w", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, nil, err
		}
		return &cert, leaf, nil
	}
	p := &certProvider{}
	if !fileExists(certPath) || !fileExists(keyPath) {
		cert, leaf, err := mkPair()
		if err != nil {
			return nil, err
		}
		p.cert.Store(cert)
		p.leaf.Store(leaf)
		return p, nil
	}
	loaded, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading the TLS certificate (delete %s to regenerate): %w", tlsCertDir(dataDir), err)
	}
	cert := &loaded
	leaf, lerr := x509.ParseCertificate(cert.Certificate[0])
	if lerr != nil {
		return nil, fmt.Errorf("parsing the TLS certificate (delete %s to regenerate): %w", tlsCertDir(dataDir), lerr)
	}
	// Rotation on boot: inside the window (or an unreadable expiry) means a
	// fresh pair, with a note saying why the browser will re-ask.
	window := tlsRotateWindow()
	if certExpiringSoon(leaf, window) {
		fmt.Fprintf(os.Stderr, "launcher: TLS certificate expires %s — inside the %s rotation window, generating a fresh pair\n",
			leaf.NotAfter.Format("2006-01-02"), window)
		cert, leaf, err = mkPair()
		if err != nil {
			return nil, err
		}
	}
	p.cert.Store(cert)
	p.leaf.Store(leaf)
	return p, nil
}

// startTLSRotator keeps the HTTPS certificate fresh for launches that stay up
// for months: once a day the leaf is checked, and inside the rotation window
// the pair is regenerated and swapped in live through certProvider — no
// restart, same behavior as the manual /api/tls/regenerate button.
func (s *Server) startTLSRotator() {
	if s.tlsProv == nil {
		return
	}
	go func() {
		window := tlsRotateWindow()
		for {
			time.Sleep(24 * time.Hour)
			if !certExpiringSoon(s.tlsProv.loadLeaf(), window) {
				continue
			}
			if _, err := s.regenServerCert(); err != nil {
				fmt.Fprintf(os.Stderr, "launcher: TLS rotation failed: %v\n", err)
				continue
			}
			if leaf := s.tlsProv.loadLeaf(); leaf != nil {
				fmt.Fprintf(os.Stderr, "launcher: TLS certificate rotated — valid until %s\n", leaf.NotAfter.Format("2006-01-02"))
			}
		}
	}()
}

// regenServerCert replaces the pair with a brand-new private key and swaps
// it into the live listener. Returns the new serial + expiry for the caller.
func (s *Server) regenServerCert() (string, error) {
	if s.tlsProv == nil {
		return "", fmt.Errorf("TLS is not enabled on this launcher")
	}
	certPEM, keyPEM, leaf, err := generateServerCert()
	if err != nil {
		return "", err
	}
	if err := writeTLSPair(s.cfg.DataDir, certPEM, keyPEM); err != nil {
		return "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return "", err
	}
	s.tlsProv.cert.Store(&cert)
	s.tlsProv.leaf.Store(leaf)
	return fmt.Sprintf("serial %x, valid until %s", leaf.SerialNumber, leaf.NotAfter.Format("2006-01-02")), nil
}

// serveTLSRegenerate answers POST /api/tls/regenerate: a fresh key + cert
// for the HTTPS listener, live. Loopback only — this is the compromise-
// response button, and a remote caller is exactly what it defends against.
func (s *Server) serveTLSRegenerate(w http.ResponseWriter, r *http.Request) {
	if !fromLocalMachine(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "certificate regeneration is only allowed from this computer"})
		return
	}
	summary, err := s.regenServerCert()
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": "new TLS certificate generated (" + summary + ") — browsers will ask you to trust it once more",
	})
}
