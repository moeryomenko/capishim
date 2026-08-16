// Package pki generates and persists the capishim pod certificate
// infrastructure: a single pod CA, leaf certificates for etcd,
// kube-apiserver, the four Cluster API provider managers and their webhook
// servers, the admin client certificate, and the service-account signing
// keypair (REQ-002, REQ-009).
package pki

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
)

const (
	// caCertName and caKeyName are the pod CA filenames under <state-dir>/pki/.
	caCertName = "ca.crt"
	caKeyName  = "ca.key"

	// saCertName and saKeyName are the service-account signing keypair
	// filenames (kubeadm-style: a PEM certificate carrying the public key).
	saCertName = "sa.pub"
	saKeyName  = "sa.key"

	// saCertSubject is the subject of the certificate that carries the SA
	// signing public key.
	saCertSubject = "service-account-signing-key"

	// webhookCertName and webhookKeyName are the filenames expected by
	// controller-runtime's webhook server inside each <comp>-webhook/ dir.
	webhookCertName = "tls.crt"
	webhookKeyName  = "tls.key"

	// keyFileMode is the permission set for every private key file.
	keyFileMode os.FileMode = 0o600
	// certFileMode is the permission set for every certificate file.
	certFileMode os.FileMode = 0o644
	// dirFileMode is the permission set for created directories.
	dirFileMode os.FileMode = 0o755

	// rsaKeySize is the modulus size for RSA keys (pod CA and SA keypair).
	rsaKeySize = 2048

	// certValidity is the lifetime of the pod CA and every issued leaf.
	certValidity = time.Hour * 24 * 3650 // ~10 years

	// maxSerialBytes bounds the random serial number space.
	maxSerialBytes = math.MaxInt64 - 1
)

// Config configures certificate inventory generation.
type Config struct {
	// StateDir is the parent state directory; all artifacts are written
	// under <StateDir>/pki/.
	StateDir string

	// BindAddress is the apiserver publish address in host:port form. The
	// host part feeds the apiserver serving certificate SANs. An empty value
	// defaults to the loopback address 127.0.0.1.
	BindAddress string
}

// Artifact is a certificate and private-key pair on disk.
type Artifact struct {
	// Name is the logical artifact name (e.g. "ca", "core-manager").
	Name string

	// CertPath is the path of the PEM-encoded certificate.
	CertPath string

	// KeyPath is the path of the PEM-encoded private key.
	KeyPath string
}

// Inventory is the full certificate inventory written by Generate. Every path
// lives under <StateDir>/pki/.
type Inventory struct {
	CA              Artifact
	EtcdServer      Artifact
	EtcdClient      Artifact
	APIServer       Artifact
	APIServerClient Artifact
	CoreManager     Artifact
	CABPKManager    Artifact
	KCPManager      Artifact
	CAPDManager     Artifact
	CoreWebhook     Artifact
	CABPKWebhook    Artifact
	KCPWebhook      Artifact
	CAPDWebhook     Artifact
	Admin           Artifact
	SAPubPath       string
	SAKeyPath       string
}

// All returns every artifact pair in a deterministic order, including the CA.
func (inv *Inventory) All() []Artifact {
	return []Artifact{
		inv.CA,
		inv.EtcdServer,
		inv.EtcdClient,
		inv.APIServer,
		inv.APIServerClient,
		inv.CoreManager,
		inv.CABPKManager,
		inv.KCPManager,
		inv.CAPDManager,
		inv.CoreWebhook,
		inv.CABPKWebhook,
		inv.KCPWebhook,
		inv.CAPDWebhook,
		inv.Admin,
	}
}

// leafSpec describes one leaf certificate to ensure on disk.
type leafSpec struct {
	artifact    Artifact
	commonName  string
	usages      []x509.ExtKeyUsage
	dnsNames    []string
	ipAddresses []net.IP
}

// Generate ensures the full certificate inventory exists under
// <StateDir>/pki/ and returns it. The pod CA is the trust anchor: a valid
// existing CA pair is reused byte-identically, a missing pair is created, and
// a corrupt or incomplete pair is an error that leaves the tree untouched.
// Leaf artifacts are written only when missing or broken; valid existing
// leaves are left byte-identical.
func Generate(ctx context.Context, cfg Config) (*Inventory, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pki: context: %w", err)
	}

	host, err := bindHost(cfg.BindAddress)
	if err != nil {
		return nil, fmt.Errorf("pki: bind address: %w", err)
	}

	dir := filepath.Join(cfg.StateDir, "pki")
	if err := os.MkdirAll(dir, dirFileMode); err != nil {
		return nil, fmt.Errorf("pki: create directory %s: %w", dir, err)
	}

	// A single clock for the whole run keeps every issued leaf from
	// outliving the CA, and matches kubeadm's ten-year certificate lifetime.
	now := time.Now()

	caCert, caKey, err := ensureCA(dir, now)
	if err != nil {
		return nil, err
	}

	inv := newInventory(dir)

	serverAuth := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	clientAuth := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	loopbackDNS := []string{"localhost"}
	loopbackIPs := []net.IP{net.ParseIP("127.0.0.1")}
	apiserverDNS, apiserverIPs := apiserverSANs(host)

	leaves := []leafSpec{
		{artifact: inv.EtcdServer, commonName: "etcd-server", usages: serverAuth, dnsNames: loopbackDNS, ipAddresses: loopbackIPs},
		{artifact: inv.EtcdClient, commonName: "etcd-client", usages: clientAuth},
		{artifact: inv.APIServer, commonName: "capishim-apiserver", usages: serverAuth, dnsNames: apiserverDNS, ipAddresses: apiserverIPs},
		{artifact: inv.APIServerClient, commonName: "capishim:apiserver-client", usages: clientAuth},
		{artifact: inv.CoreManager, commonName: "capishim:core-manager", usages: clientAuth},
		{artifact: inv.CABPKManager, commonName: "capishim:cabpk-manager", usages: clientAuth},
		{artifact: inv.KCPManager, commonName: "capishim:kcp-manager", usages: clientAuth},
		{artifact: inv.CAPDManager, commonName: "capishim:capd-manager", usages: clientAuth},
		{artifact: inv.CoreWebhook, commonName: "capishim:core-webhook", usages: serverAuth, dnsNames: loopbackDNS, ipAddresses: loopbackIPs},
		{artifact: inv.CABPKWebhook, commonName: "capishim:cabpk-webhook", usages: serverAuth, dnsNames: loopbackDNS, ipAddresses: loopbackIPs},
		{artifact: inv.KCPWebhook, commonName: "capishim:kcp-webhook", usages: serverAuth, dnsNames: loopbackDNS, ipAddresses: loopbackIPs},
		{artifact: inv.CAPDWebhook, commonName: "capishim:capd-webhook", usages: serverAuth, dnsNames: loopbackDNS, ipAddresses: loopbackIPs},
		{artifact: inv.Admin, commonName: "capishim:admin", usages: clientAuth},
	}
	for _, spec := range leaves {
		if err := ensureLeaf(spec, caCert, caKey, now); err != nil {
			return nil, err
		}
	}

	inv.SAPubPath = filepath.Join(dir, saCertName)
	inv.SAKeyPath = filepath.Join(dir, saKeyName)
	if err := ensureServiceAccountPair(inv.SAPubPath, inv.SAKeyPath); err != nil {
		return nil, err
	}

	return inv, nil
}

// newInventory builds the inventory paths for the given pki directory.
func newInventory(dir string) *Inventory {
	artifact := func(name, certFile, keyFile string) Artifact {
		return Artifact{
			Name:     name,
			CertPath: filepath.Join(dir, certFile),
			KeyPath:  filepath.Join(dir, keyFile),
		}
	}
	webhook := func(comp string) Artifact {
		return artifact(comp+"-webhook", filepath.Join(comp+"-webhook", webhookCertName), filepath.Join(comp+"-webhook", webhookKeyName))
	}
	return &Inventory{
		CA:              artifact("ca", caCertName, caKeyName),
		EtcdServer:      artifact("etcd-server", "etcd-server.crt", "etcd-server.key"),
		EtcdClient:      artifact("etcd-client", "etcd-client.crt", "etcd-client.key"),
		APIServer:       artifact("apiserver", "apiserver.crt", "apiserver.key"),
		APIServerClient: artifact("apiserver-client", "apiserver-client.crt", "apiserver-client.key"),
		CoreManager:     artifact("core-manager", "core-manager.crt", "core-manager.key"),
		CABPKManager:    artifact("cabpk-manager", "cabpk-manager.crt", "cabpk-manager.key"),
		KCPManager:      artifact("kcp-manager", "kcp-manager.crt", "kcp-manager.key"),
		CAPDManager:     artifact("capd-manager", "capd-manager.crt", "capd-manager.key"),
		CoreWebhook:     webhook("core"),
		CABPKWebhook:    webhook("cabpk"),
		KCPWebhook:      webhook("kcp"),
		CAPDWebhook:     webhook("capd"),
		Admin:           artifact("admin", "admin.crt", "admin.key"),
	}
}

// bindHost extracts the host part of a host:port bind address. An empty bind
// address maps to the loopback default; a malformed address (missing port or
// empty host) is an error.
func bindHost(bindAddress string) (string, error) {
	if bindAddress == "" {
		return "", nil
	}
	host, _, err := net.SplitHostPort(bindAddress)
	if err != nil {
		return "", fmt.Errorf("invalid bind address %q: %w", bindAddress, err)
	}
	if host == "" {
		return "", fmt.Errorf("invalid bind address %q: empty host", bindAddress)
	}
	return host, nil
}

// apiserverSANs returns the DNS and IP SANs for the apiserver serving
// certificate: localhost and the loopback address plus the configured bind
// host. A wildcard bind host (0.0.0.0 or ::) is not a reachable address and
// is never added.
func apiserverSANs(host string) ([]string, []net.IP) {
	dnsNames := []string{"localhost"}
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1")}
	if host == "" {
		return dnsNames, ipAddresses
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsUnspecified() {
			ipAddresses = appendUniqueIP(ipAddresses, ip)
		}
		return dnsNames, ipAddresses
	}
	return appendUniqueDNS(dnsNames, host), ipAddresses
}

// appendUniqueIP appends ip to ips unless it is already present.
func appendUniqueIP(ips []net.IP, ip net.IP) []net.IP {
	for _, have := range ips {
		if have.Equal(ip) {
			return ips
		}
	}
	return append(ips, ip)
}

// appendUniqueDNS appends name to dns unless it is already present.
func appendUniqueDNS(dns []string, name string) []string {
	for _, have := range dns {
		if have == name {
			return dns
		}
	}
	return append(dns, name)
}

// ensureCA returns the pod CA from disk, or creates and persists a new one.
// An existing valid pair is reused; a missing pair is generated; a corrupt or
// incomplete pair is an error so the trust anchor is never silently replaced.
func ensureCA(dir string, now time.Time) (*x509.Certificate, crypto.Signer, error) {
	certPath := filepath.Join(dir, caCertName)
	keyPath := filepath.Join(dir, caKeyName)

	certExists, err := fileExists(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: check CA certificate: %w", err)
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: check CA key: %w", err)
	}

	if certExists && keyExists {
		caCert, caKey, err := loadCAPair(certPath, keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("pki: existing CA pair is invalid: %w", err)
		}
		return caCert, caKey, nil
	}
	if certExists || keyExists {
		return nil, nil, errors.New("pki: CA pair is incomplete; refusing to regenerate a partial trust anchor")
	}

	caKey, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: generate CA key: %w", err)
	}
	caCert, err := newCACert(caKey, now)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: generate CA certificate: %w", err)
	}
	if err := writeCertFile(certPath, caCert); err != nil {
		return nil, nil, fmt.Errorf("pki: write CA certificate: %w", err)
	}
	if err := writeKeyFile(keyPath, caKey); err != nil {
		return nil, nil, fmt.Errorf("pki: write CA key: %w", err)
	}
	return caCert, caKey, nil
}

// loadCAPair loads and validates the persisted pod CA. The pair must parse,
// the key must match the certificate, and the certificate must be a CA.
func loadCAPair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	caCert, err := readSingleCert(certPath)
	if err != nil {
		return nil, nil, err
	}
	caKey, err := readSigner(keyPath)
	if err != nil {
		return nil, nil, err
	}
	if !publicKeysEqual(caKey.Public(), caCert.PublicKey) {
		return nil, nil, errors.New("CA key does not match CA certificate")
	}
	if !caCert.IsCA || !caCert.BasicConstraintsValid || caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("CA certificate is not a certificate authority")
	}
	return caCert, caKey, nil
}

// newCACert builds the self-signed pod CA certificate.
func newCACert(key crypto.Signer, now time.Time) (*x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "capishim-ca"},
		NotBefore:             now.UTC(),
		NotAfter:              now.Add(certValidity).UTC(),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	return createCertificate(tmpl, tmpl, key.Public(), key)
}

// newSignedCert builds a leaf certificate signed by the pod CA.
func newSignedCert(
	cfg cert.Config,
	now time.Time,
	key crypto.Signer,
	caCert *x509.Certificate,
	caKey crypto.Signer,
) (*x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cfg.CommonName, Organization: cfg.Organization},
		NotBefore:             now.UTC(),
		NotAfter:              now.Add(certValidity).UTC(),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           cfg.Usages,
		BasicConstraintsValid: true,
		DNSNames:              cfg.AltNames.DNSNames,
		IPAddresses:           cfg.AltNames.IPs,
	}
	return createCertificate(tmpl, caCert, key.Public(), caKey)
}

// publicKeyCert builds a self-signed certificate that carries the public half
// of the SA signing key, the format kube-apiserver accepts for
// --service-account-key-file (kubeadm-style sa.pub).
func publicKeyCert(publicKey *rsa.PublicKey, signingKey *rsa.PrivateKey) (*x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: saCertSubject},
		NotBefore:             now.UTC(),
		NotAfter:              now.Add(certValidity).UTC(),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	return createCertificate(tmpl, tmpl, publicKey, signingKey)
}

// createCertificate signs a certificate template and parses the result.
func createCertificate(tmpl, parent *x509.Certificate, publicKey crypto.PublicKey, signer crypto.Signer) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, publicKey, signer)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return parsed, nil
}

// randomSerial returns a positive random serial number in [1, MaxInt64-1].
func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, big.NewInt(maxSerialBytes))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

// ensureLeaf writes spec's certificate pair unless a valid pair already
// exists on disk. A missing or broken pair is regenerated consistently so the
// certificate and key always match and chain to the pod CA.
func ensureLeaf(spec leafSpec, caCert *x509.Certificate, caKey crypto.Signer, now time.Time) error {
	if validPair(spec.artifact.CertPath, spec.artifact.KeyPath) {
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("pki: generate key for %s: %w", spec.artifact.Name, err)
	}
	cfg := cert.Config{
		CommonName: spec.commonName,
		AltNames: cert.AltNames{
			DNSNames: spec.dnsNames,
			IPs:      spec.ipAddresses,
		},
		Usages: spec.usages,
	}
	leaf, err := newSignedCert(cfg, now, key, caCert, caKey)
	if err != nil {
		return fmt.Errorf("pki: sign certificate for %s: %w", spec.artifact.Name, err)
	}
	if err := writeCertFile(spec.artifact.CertPath, leaf); err != nil {
		return fmt.Errorf("pki: write certificate for %s: %w", spec.artifact.Name, err)
	}
	if err := writeKeyFile(spec.artifact.KeyPath, key); err != nil {
		return fmt.Errorf("pki: write key for %s: %w", spec.artifact.Name, err)
	}
	return nil
}

// ensureServiceAccountPair writes the RSA service-account signing keypair
// (kubeadm-style sa.key + sa.pub) unless a valid pair already exists.
func ensureServiceAccountPair(certPath, keyPath string) error {
	if validSAPair(certPath, keyPath) {
		return nil
	}

	key, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return fmt.Errorf("pki: generate service-account key: %w", err)
	}
	if err := writeKeyFile(keyPath, key); err != nil {
		return fmt.Errorf("pki: write service-account key: %w", err)
	}
	pubCert, err := publicKeyCert(&key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("pki: generate service-account public key certificate: %w", err)
	}
	if err := writeCertFile(certPath, pubCert); err != nil {
		return fmt.Errorf("pki: write service-account public key certificate: %w", err)
	}
	return nil
}

// validPair reports whether the certificate and key pair on disk is parseable
// and the key matches the certificate.
func validPair(certPath, keyPath string) bool {
	certs, err := readSingleCert(certPath)
	if err != nil {
		return false
	}
	key, err := readSigner(keyPath)
	if err != nil {
		return false
	}
	return publicKeysEqual(key.Public(), certs.PublicKey)
}

// validSAPair reports whether the SA keypair on disk is a valid RSA keypair
// whose certificate matches the key.
func validSAPair(certPath, keyPath string) bool {
	if !validPair(certPath, keyPath) {
		return false
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return false
	}
	key, err := keyutil.ParsePrivateKeyPEM(keyData)
	if err != nil {
		return false
	}
	_, ok := key.(*rsa.PrivateKey)
	return ok
}

// fileExists reports whether path names an existing regular file.
func fileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info.Mode().IsRegular(), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// readSingleCert reads a PEM file and returns its first certificate.
func readSingleCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	certs, err := cert.ParseCertsPEM(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return certs[0], nil
}

// readSigner reads a PEM file and returns the private key it contains as a
// crypto.Signer.
func readSigner(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	key, err := keyutil.ParsePrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s does not contain a usable private key", path)
	}
	return signer, nil
}

// writeCertFile persists a certificate PEM with 0644 permissions.
func writeCertFile(path string, c *x509.Certificate) error {
	data, err := cert.EncodeCertificates(c)
	if err != nil {
		return fmt.Errorf("encode certificate for %s: %w", path, err)
	}
	return writeFile(path, data, certFileMode)
}

// writeKeyFile persists a private key PEM with 0600 permissions.
func writeKeyFile(path string, key crypto.Signer) error {
	data, err := keyutil.MarshalPrivateKeyToPEM(key)
	if err != nil {
		return fmt.Errorf("encode key for %s: %w", path, err)
	}
	return writeFile(path, data, keyFileMode)
}

// writeFile writes data to path with the given mode, chmodding explicitly so
// the process umask cannot weaken the permission set.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirFileMode); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set permissions on %s: %w", path, err)
	}
	return nil
}

// publicKeysEqual reports whether two public keys encode identically.
func publicKeysEqual(a, b crypto.PublicKey) bool {
	aBytes, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}
	bBytes, err := x509.MarshalPKIXPublicKey(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aBytes, bBytes)
}
