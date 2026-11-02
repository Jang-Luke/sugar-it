// Package identity manages the host's self-signed Ed25519 certificate. The
// host ID is derived from the public key, so it survives certificate renewal.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sugarintegration/internal/fsutil"
)

const (
	certFile = "identity.crt"
	keyFile  = "identity.key"
	validFor = 20 * 365 * 24 * time.Hour
)

type Identity struct {
	Cert tls.Certificate
	ID   string
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDOf returns the lower-case base32 SHA-256 of the certificate's SPKI.
func IDOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return strings.ToLower(b32.EncodeToString(sum[:]))
}

// LoadOrCreate loads the identity stored in dir, creating it if the key does
// not exist yet. An unreadable or corrupt key is an error and is never
// replaced, since that would silently change the host's ID.
func LoadOrCreate(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyPath, certPath := filepath.Join(dir, keyFile), filepath.Join(dir, certFile)

	keyPEM, err := os.ReadFile(keyPath)
	var priv ed25519.PrivateKey
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, cerr := os.Stat(certPath); cerr == nil {
			return nil, fmt.Errorf("identity: %s exists but %s is missing", certFile, keyFile)
		}
		_, priv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := fsutil.WriteAtomic(keyPath, keyPEM, 0o600); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		priv, err = parseKey(keyPEM)
		if err != nil {
			return nil, fmt.Errorf("identity: %s: %w", keyFile, err)
		}
	}

	certPEM, err := os.ReadFile(certPath)
	if errors.Is(err, os.ErrNotExist) {
		// Key survived but certificate did not: reissue; the ID is unchanged.
		certPEM, err = newCertPEM(priv)
		if err == nil {
			err = fsutil.WriteAtomic(certPath, certPEM, 0o600)
		}
	}
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	pair.Leaf = leaf
	return &Identity{Cert: pair, ID: IDOf(leaf)}, nil
}

func parseKey(keyPEM []byte) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return priv, nil
}

func newCertPEM(priv ed25519.PrivateKey) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "sugar-it"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
