package shim

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// unboundCN is the client-certificate CN of the minted unbound identity. The
// setup container binds only the manager CNs and capishim:admin to RBAC
// roles, so this identity matches no rule and receives Forbidden on every
// resource (REQ-004, VC-03).
const unboundCN = "capishim:unbound"

// unboundValidity is the lifetime of the minted client certificate.
const unboundValidity = 24 * time.Hour

// MintUnboundClientCert signs a fresh client certificate with the pod CA
// (stateDir/pki/ca.crt + ca.key) for an identity that no RBAC rule binds, and
// writes a kubeconfig that authenticates as that identity. The returned path
// is an existing kubeconfig file; VC-03 uses it to assert the management
// apiserver answers the unbound identity with Forbidden on list clusters.
func MintUnboundClientCert(ctx context.Context, kubeconfigPath, stateDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("shim: mint unbound cert: %w", ctx.Err())
	}

	caCert, caKey, err := loadCA(stateDir)
	if err != nil {
		return "", err
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("shim: generate unbound client key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: unboundCN},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(unboundValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, priv.Public(), caKey)
	if err != nil {
		return "", fmt.Errorf("shim: sign unbound client certificate: %w", err)
	}

	outDir := filepath.Join(stateDir, "e2e")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", fmt.Errorf("shim: create e2e directory %s: %w", outDir, err)
	}
	certPath := filepath.Join(outDir, "unbound.crt")
	keyPath := filepath.Join(outDir, "unbound.key")
	if err := writePEMFile(certPath, "CERTIFICATE", der); err != nil {
		return "", err
	}
	if err := writePEMFile(keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(priv)); err != nil {
		return "", err
	}

	// Mirror the admin kubeconfig (same server and CA) but authenticate with
	// the unbound certificate pair.
	admin, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("shim: load admin kubeconfig: %w", err)
	}
	kc := admin.DeepCopy()
	for _, info := range kc.AuthInfos {
		info.ClientCertificate = certPath
		info.ClientKey = keyPath
		info.ClientCertificateData = nil
		info.ClientKeyData = nil
	}
	data, err := clientcmd.Write(*kc)
	if err != nil {
		return "", fmt.Errorf("shim: serialize unbound kubeconfig: %w", err)
	}
	out := filepath.Join(outDir, "unbound.kubeconfig")
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return "", fmt.Errorf("shim: write unbound kubeconfig %s: %w", out, err)
	}
	return out, nil
}

// loadCA reads the pod CA certificate and private key from the state pki
// directory.
func loadCA(stateDir string) (*x509.Certificate, *rsa.PrivateKey, error) {
	caPEM, err := os.ReadFile(filepath.Join(stateDir, "pki", "ca.crt"))
	if err != nil {
		return nil, nil, fmt.Errorf("shim: read pod CA certificate: %w", err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("shim: pod CA certificate is not PEM data")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("shim: parse pod CA certificate: %w", err)
	}

	keyPEM, err := os.ReadFile(filepath.Join(stateDir, "pki", "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("shim: read pod CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("shim: pod CA key is not PEM data")
	}
	caKey, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("shim: parse pod CA key: %w", err)
	}
	return caCert, caKey, nil
}

// parsePrivateKey parses an RSA private key in PKCS1 or PKCS8 form (the pki
// package writes PKCS1 via k8s keyutil; PKCS8 is accepted defensively).
func parsePrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("pod CA key is %T, want *rsa.PrivateKey", key)
		}
		return rsaKey, nil
	}
	return nil, fmt.Errorf("pod CA key is neither PKCS1 nor PKCS8 RSA")
}

// randomSerial returns a positive 128-bit serial number for a signed
// certificate.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("shim: generate certificate serial: %w", err)
	}
	return serial, nil
}

// writePEMFile writes a single PEM block to path with 0600 permissions.
func writePEMFile(path, blockType string, der []byte) error {
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("shim: write %s: %w", path, err)
	}
	return nil
}
