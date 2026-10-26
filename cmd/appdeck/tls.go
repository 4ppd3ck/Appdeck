// Free TLS for the LAN dashboard: a self-signed certificate generated on
// first boot (or on demand) and served on a second, HTTPS-only port. The
// plain-HTTP listener stays untouched — the native window and loopback
// browsing keep using it, since loopback traffic never crosses the network,
// while remote/Wi-Fi browsers switch to https://<lan-ip>:<tls-port>.
//
// The certificate costs nothing, needs no CA, and never expires the
// launcher: it lives in <data>/tls/{cert,key}.pem and is regenerated with a
// fresh private key by POST /api/tls/regenerate (loopback only) — the
// "I think I'm compromised" button. A browser that clicked through the old
// certificate warns again on the new one; deleting <data>/tls and
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
// GetCertificate, so a regeneration swaps it in live — no restart.
type certProvider struct {
	cert atomic.Pointer[tls.Certificate]
}

func (p *certProvider) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := p.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("no TLS certificate loaded")
	}
	return c, nil
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

// generateServerCert creates a fresh self-signed pair (new private key).
func generateServerCert() (certPEM, keyPEM []byte, leaf *x509.Certificate, err error) {
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
		NotAfter:              now.Add(397 * 24 * time.Hour),
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

// ensureServerCert loads <data>/tls, generating the pair on first boot.
func ensureServerCert(dataDir string) (*certProvider, error) {
	certPath := filepath.Join(tlsCertDir(dataDir), "cert.pem")
	keyPath := filepath.Join(tlsCertDir(dataDir), "key.pem")
	if !fileExists(certPath) || !fileExists(keyPath) {
		certPEM, keyPEM, _, err := generateServerCert()
		if err != nil {
			return nil, fmt.Errorf("generating the TLS certificate: %v", err)
		}
		if err := writeTLSPair(dataDir, certPEM, keyPEM); err != nil {
			return nil, fmt.Errorf("writing the TLS certificate: %v", err)
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading the TLS certificate (delete %s to regenerate): %v", tlsCertDir(dataDir), err)
	}
	p := &certProvider{}
	p.cert.Store(&cert)
	return p, nil
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
