package pkg

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The autoscaler sends its GitHub token to api.github.com through Go's default
// TLS verifier, so the toolchain it is built with decides whether a forged
// certificate is accepted. Go 1.26.10 and 1.27.3 reject any chain containing a
// compromised eMudhra intermediate's key (CVE-2026-111943), even under a root
// that still trusts it; older toolchains accept the chain.
//
// The leaf was signed with the compromised EM DV TLS CA - G3B key and is
// served by the incident's public test host.
func TestVerifyRejectsCompromisedEmudhraChain(t *testing.T) {
	roots := x509.NewCertPool()
	roots.AddCert(loadEmudhraFixture(t, "emsign-ecc-root-ca-g3.pem"))
	intermediates := x509.NewCertPool()
	intermediates.AddCert(loadEmudhraFixture(t, "em-dv-tls-ca-g3b.pem"))
	intermediates.AddCert(loadEmudhraFixture(t, "emsign-root-tls-ca-g3-cross.pem"))
	leaf := loadEmudhraFixture(t, "leaf.pem")

	_, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       "dv-g3b-emudhra.we-have-a-problem.com",
		Roots:         roots,
		Intermediates: intermediates,
		// Inside the leaf's validity window, so expiry cannot be the reason.
		CurrentTime: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
	})

	if err == nil {
		t.Fatal("chain through a compromised eMudhra intermediate verified; build with Go 1.26.10+ or 1.27.3+")
	}
	// The distrust surfaces as NoValidChains. A broken or incomplete fixture
	// fails differently (UnknownAuthorityError, a name or expiry error), and
	// accepting those would let this pass on a vulnerable toolchain.
	var invalidErr x509.CertificateInvalidError
	if !errors.As(err, &invalidErr) || invalidErr.Reason != x509.NoValidChains {
		t.Fatalf("verification failed for a reason other than the distrusted keys: %v", err)
	}
}

func loadEmudhraFixture(t *testing.T, name string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "emudhra", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("fixture %s holds no PEM block", name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return cert
}
