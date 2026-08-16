package pki_test

import (
	"crypto/rsa"
	"crypto/x509"
	"net"
	"testing"
)

func assertDNSName(t *testing.T, cert *x509.Certificate, want string) {
	t.Helper()
	for _, dns := range cert.DNSNames {
		if dns == want {
			return
		}
	}
	t.Errorf("certificate %s missing DNS SAN %q; have %v", cert.Subject.CommonName, want, cert.DNSNames)
}

func assertIP(t *testing.T, cert *x509.Certificate, want net.IP) {
	t.Helper()
	for _, ip := range cert.IPAddresses {
		if ip.Equal(want) {
			return
		}
	}
	t.Errorf("certificate %s missing IP SAN %s; have %v", cert.Subject.CommonName, want, cert.IPAddresses)
}

func assertExtKeyUsage(t *testing.T, cert *x509.Certificate, want x509.ExtKeyUsage) {
	t.Helper()
	for _, usage := range cert.ExtKeyUsage {
		if usage == want {
			return
		}
	}
	t.Errorf("certificate %s missing ExtKeyUsage %v; have %v", cert.Subject.CommonName, want, cert.ExtKeyUsage)
}

// TestApiserverServingSANs asserts the apiserver serving cert always carries
// DNS localhost and IP 127.0.0.1, plus the configured bind host as an IP SAN
// when it is an IP and as a DNS SAN when it is a hostname. A wildcard bind of
// 0.0.0.0 is not a reachable address and must not appear as a SAN.
func TestApiserverServingSANs(t *testing.T) {
	t.Parallel()

	ip127 := net.ParseIP("127.0.0.1")
	tests := []struct {
		name       string
		bind       string
		wantDNS    []string
		wantIPs    []net.IP
		excludeIPs []net.IP
	}{
		{"default loopback", defaultBind, []string{"localhost"}, []net.IP{ip127}, nil},
		{"custom IP", "10.20.30.40:6443", []string{"localhost"}, []net.IP{ip127, net.ParseIP("10.20.30.40")}, nil},
		{"hostname", "capishim.test:6443", []string{"localhost", "capishim.test"}, []net.IP{ip127}, nil},
		{"wildcard bind", "0.0.0.0:6443", []string{"localhost"}, []net.IP{ip127}, []net.IP{net.IPv4zero}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			inv := generate(t, stateDir, tt.bind)
			cert := loadCert(t, inv.APIServer.CertPath)

			for _, dns := range tt.wantDNS {
				assertDNSName(t, cert, dns)
			}
			for _, ip := range tt.wantIPs {
				assertIP(t, cert, ip)
			}
			for _, ip := range tt.excludeIPs {
				for _, have := range cert.IPAddresses {
					if have.Equal(ip) {
						t.Errorf("apiserver cert must not contain IP SAN %s; have %v", ip, cert.IPAddresses)
					}
				}
			}
		})
	}
}

// TestWebhookServingSANs asserts every per-component webhook serving cert
// carries DNS localhost and IP 127.0.0.1, matching the rewritten webhook URLs
// https://localhost:<port>.
func TestWebhookServingSANs(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)
	ip127 := net.ParseIP("127.0.0.1")

	tests := []struct {
		name     string
		certPath string
	}{
		{"core", inv.CoreWebhook.CertPath},
		{"cabpk", inv.CABPKWebhook.CertPath},
		{"kcp", inv.KCPWebhook.CertPath},
		{"capd", inv.CAPDWebhook.CertPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cert := loadCert(t, tt.certPath)
			assertDNSName(t, cert, "localhost")
			assertIP(t, cert, ip127)
		})
	}
}

// TestEtcdServerSANs asserts the etcd server cert serves the loopback client
// TLS endpoint (plan assumption 8: client TLS on 127.0.0.1:2379).
func TestEtcdServerSANs(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	cert := loadCert(t, inv.EtcdServer.CertPath)
	assertDNSName(t, cert, "localhost")
	assertIP(t, cert, net.ParseIP("127.0.0.1"))
}

// TestCertCNs asserts the exact CommonName of every certificate, including
// the four RBAC-bearing manager identities and the admin identity.
func TestCertCNs(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	tests := []struct {
		name     string
		certPath string
		wantCN   string
	}{
		{"ca", inv.CA.CertPath, "capishim-ca"},
		{"etcd-server", inv.EtcdServer.CertPath, "etcd-server"},
		{"etcd-client", inv.EtcdClient.CertPath, "etcd-client"},
		{"apiserver", inv.APIServer.CertPath, "capishim-apiserver"},
		{"apiserver-client", inv.APIServerClient.CertPath, "capishim:apiserver-client"},
		{"core-manager", inv.CoreManager.CertPath, "capishim:core-manager"},
		{"cabpk-manager", inv.CABPKManager.CertPath, "capishim:cabpk-manager"},
		{"kcp-manager", inv.KCPManager.CertPath, "capishim:kcp-manager"},
		{"capd-manager", inv.CAPDManager.CertPath, "capishim:capd-manager"},
		{"core-webhook", inv.CoreWebhook.CertPath, "capishim:core-webhook"},
		{"cabpk-webhook", inv.CABPKWebhook.CertPath, "capishim:cabpk-webhook"},
		{"kcp-webhook", inv.KCPWebhook.CertPath, "capishim:kcp-webhook"},
		{"capd-webhook", inv.CAPDWebhook.CertPath, "capishim:capd-webhook"},
		{"admin", inv.Admin.CertPath, "capishim:admin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cert := loadCert(t, tt.certPath)
			if got := cert.Subject.CommonName; got != tt.wantCN {
				t.Errorf("CN = %q, want %q", got, tt.wantCN)
			}
		})
	}
}

// TestCertKeyUsage asserts serving certs carry serverAuth and client certs
// carry clientAuth.
func TestCertKeyUsage(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	serving := []struct {
		name     string
		certPath string
	}{
		{"etcd-server", inv.EtcdServer.CertPath},
		{"apiserver", inv.APIServer.CertPath},
		{"core-webhook", inv.CoreWebhook.CertPath},
		{"cabpk-webhook", inv.CABPKWebhook.CertPath},
		{"kcp-webhook", inv.KCPWebhook.CertPath},
		{"capd-webhook", inv.CAPDWebhook.CertPath},
	}
	for _, tt := range serving {
		t.Run(tt.name+" serverAuth", func(t *testing.T) {
			t.Parallel()
			assertExtKeyUsage(t, loadCert(t, tt.certPath), x509.ExtKeyUsageServerAuth)
		})
	}

	clients := []struct {
		name     string
		certPath string
	}{
		{"etcd-client", inv.EtcdClient.CertPath},
		{"apiserver-client", inv.APIServerClient.CertPath},
		{"core-manager", inv.CoreManager.CertPath},
		{"cabpk-manager", inv.CABPKManager.CertPath},
		{"kcp-manager", inv.KCPManager.CertPath},
		{"capd-manager", inv.CAPDManager.CertPath},
		{"admin", inv.Admin.CertPath},
	}
	for _, tt := range clients {
		t.Run(tt.name+" clientAuth", func(t *testing.T) {
			t.Parallel()
			assertExtKeyUsage(t, loadCert(t, tt.certPath), x509.ExtKeyUsageClientAuth)
		})
	}
}

// TestSAKeypair asserts the SA signing keypair exists as an RSA key (sa.key)
// and a PEM certificate (sa.pub) whose public key matches the private key.
func TestSAKeypair(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	key := loadKey(t, inv.SAKeyPath)
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("sa.key has type %T, want *rsa.PrivateKey", key)
	}
	pub := loadCert(t, inv.SAPubPath)
	if !publicKeysEqual(t, rsaKey.Public(), pub.PublicKey) {
		t.Error("sa.pub does not match sa.key")
	}
}

// TestCertValidity asserts the CA and every leaf are valid for approximately
// ten years.
func TestCertValidity(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	assertValidity(t, loadCert(t, inv.CA.CertPath))

	tests := []struct {
		name     string
		certPath string
	}{
		{"etcd-server", inv.EtcdServer.CertPath},
		{"etcd-client", inv.EtcdClient.CertPath},
		{"apiserver", inv.APIServer.CertPath},
		{"apiserver-client", inv.APIServerClient.CertPath},
		{"core-manager", inv.CoreManager.CertPath},
		{"cabpk-manager", inv.CABPKManager.CertPath},
		{"kcp-manager", inv.KCPManager.CertPath},
		{"capd-manager", inv.CAPDManager.CertPath},
		{"core-webhook", inv.CoreWebhook.CertPath},
		{"cabpk-webhook", inv.CABPKWebhook.CertPath},
		{"kcp-webhook", inv.KCPWebhook.CertPath},
		{"capd-webhook", inv.CAPDWebhook.CertPath},
		{"admin", inv.Admin.CertPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertValidity(t, loadCert(t, tt.certPath))
		})
	}
}

// TestKeyTypes asserts every leaf key and the CA key is RSA or ECDSA.
func TestKeyTypes(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	inv := generate(t, stateDir, defaultBind)

	tests := []struct {
		name string
		path string
	}{
		{"ca", inv.CA.KeyPath},
		{"etcd-server", inv.EtcdServer.KeyPath},
		{"etcd-client", inv.EtcdClient.KeyPath},
		{"apiserver", inv.APIServer.KeyPath},
		{"apiserver-client", inv.APIServerClient.KeyPath},
		{"core-manager", inv.CoreManager.KeyPath},
		{"cabpk-manager", inv.CABPKManager.KeyPath},
		{"kcp-manager", inv.KCPManager.KeyPath},
		{"capd-manager", inv.CAPDManager.KeyPath},
		{"core-webhook", inv.CoreWebhook.KeyPath},
		{"cabpk-webhook", inv.CABPKWebhook.KeyPath},
		{"kcp-webhook", inv.KCPWebhook.KeyPath},
		{"capd-webhook", inv.CAPDWebhook.KeyPath},
		{"admin", inv.Admin.KeyPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertRSAOrECDSA(t, loadKey(t, tt.path))
		})
	}
}
