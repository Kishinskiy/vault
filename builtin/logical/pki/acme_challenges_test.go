package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/vault/builtin/logical/pki/dnstest"

	"github.com/stretchr/testify/require"
)

type keyAuthorizationTestCase struct {
	keyAuthz   string
	token      string
	thumbprint string
	shouldFail bool
}

var keyAuthorizationTestCases = []keyAuthorizationTestCase{
	{
		// Entirely empty
		"",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Both empty
		".",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Not equal
		"non-.non-",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Empty thumbprint
		"non-.",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Empty token
		".non-",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Wrong order
		"non-empty-thumbprint.non-empty-token",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Too many pieces
		"one.two.three",
		"non-empty-token",
		"non-empty-thumbprint",
		true,
	},
	{
		// Valid
		"non-empty-token.non-empty-thumbprint",
		"non-empty-token",
		"non-empty-thumbprint",
		false,
	},
}

func TestAcmeValidateKeyAuthorization(t *testing.T) {
	t.Parallel()

	for index, tc := range keyAuthorizationTestCases {
		isValid, err := ValidateKeyAuthorization(tc.keyAuthz, tc.token, tc.thumbprint)
		if !isValid && err == nil {
			t.Fatalf("[%d] expected failure to give reason via err (%v / %v)", index, isValid, err)
		}

		expectedValid := !tc.shouldFail
		if expectedValid != isValid {
			t.Fatalf("[%d] got ret=%v, expected ret=%v (shouldFail=%v)", index, isValid, expectedValid, tc.shouldFail)
		}
	}
}

func TestAcmeValidateHTTP01Challenge(t *testing.T) {
	t.Parallel()

	for index, tc := range keyAuthorizationTestCases {
		validFunc := func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(tc.keyAuthz))
		}
		withPadding := func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("  " + tc.keyAuthz + "     "))
		}
		withRedirect := func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/.well-known/") {
				http.Redirect(w, r, "/my-http-01-challenge-response", 301)
				return
			}

			w.Write([]byte(tc.keyAuthz))
		}
		withSleep := func(w http.ResponseWriter, r *http.Request) {
			// Long enough to ensure any excessively short timeouts are hit,
			// not long enough to trigger a failure (hopefully).
			time.Sleep(5 * time.Second)
			w.Write([]byte(tc.keyAuthz))
		}

		validHandlers := []http.HandlerFunc{
			http.HandlerFunc(validFunc), http.HandlerFunc(withPadding),
			http.HandlerFunc(withRedirect), http.HandlerFunc(withSleep),
		}

		for handlerIndex, handler := range validHandlers {
			func() {
				ts := httptest.NewServer(handler)
				defer ts.Close()

				host := ts.URL[7:]
				isValid, err := ValidateHTTP01Challenge(host, tc.token, tc.thumbprint, &acmeConfigEntry{})
				if !isValid && err == nil {
					t.Fatalf("[tc=%d/handler=%d] expected failure to give reason via err (%v / %v)", index, handlerIndex, isValid, err)
				}

				expectedValid := !tc.shouldFail
				if expectedValid != isValid {
					t.Fatalf("[tc=%d/handler=%d] got ret=%v (err=%v), expected ret=%v (shouldFail=%v)", index, handlerIndex, isValid, err, expectedValid, tc.shouldFail)
				}
			}()
		}
	}

	// Negative test cases for various HTTP-specific scenarios.
	redirectLoop := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/my-http-01-challenge-response", 301)
	}
	publicRedirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://hashicorp.com/", 301)
	}
	noData := func(w http.ResponseWriter, r *http.Request) {}
	noContent := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
	notFound := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}
	simulateHang := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
		w.Write([]byte("my-token.my-thumbprint"))
	}
	tooLarge := func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 512; i++ {
			w.Write([]byte("my-token.my-thumbprint\n"))
		}
	}

	validHandlers := []http.HandlerFunc{
		http.HandlerFunc(redirectLoop), http.HandlerFunc(publicRedirect),
		http.HandlerFunc(noData), http.HandlerFunc(noContent),
		http.HandlerFunc(notFound), http.HandlerFunc(simulateHang),
		http.HandlerFunc(tooLarge),
	}
	for handlerIndex, handler := range validHandlers {
		func() {
			ts := httptest.NewServer(handler)
			defer ts.Close()

			host := ts.URL[7:]
			isValid, err := ValidateHTTP01Challenge(host, "my-token", "my-thumbprint", &acmeConfigEntry{})
			if isValid || err == nil {
				t.Fatalf("[handler=%d] expected failure validating challenge (%v / %v)", handlerIndex, isValid, err)
			}
		}()
	}
}

func TestAcmeValidateDNS01Challenge(t *testing.T) {
	t.Parallel()

	host := "dadgarcorp.com"
	resolver := dnstest.SetupResolver(t, host)
	defer resolver.Cleanup()

	t.Logf("DNS Server Address: %v", resolver.GetLocalAddr())

	config := &acmeConfigEntry{
		DNSResolver: resolver.GetLocalAddr(),
	}

	for index, tc := range keyAuthorizationTestCases {
		checksum := sha256.Sum256([]byte(tc.keyAuthz))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])
		resolver.AddRecord(DNSChallengePrefix+host, "TXT", authz)
		resolver.PushConfig()

		isValid, err := ValidateDNS01Challenge(host, tc.token, tc.thumbprint, config)
		if !isValid && err == nil {
			t.Fatalf("[tc=%d] expected failure to give reason via err (%v / %v)", index, isValid, err)
		}

		expectedValid := !tc.shouldFail
		if expectedValid != isValid {
			t.Fatalf("[tc=%d] got ret=%v (err=%v), expected ret=%v (shouldFail=%v)", index, isValid, err, expectedValid, tc.shouldFail)
		}

		resolver.RemoveAllRecords()
	}
}

func TestAcmeValidateTLSALPN01Challenge(t *testing.T) {
	// This test is not parallel because we modify ALPNPort to use a custom
	// non-standard port _just for testing purposes_.
	host := "localhost"
	config := &acmeConfigEntry{}

	returnedProtocols := []string{ALPNProtocol}
	var certificates []*x509.Certificate
	var privateKey crypto.PrivateKey

	tlsCfg := &tls.Config{}
	tlsCfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		var retCfg tls.Config = *tlsCfg
		retCfg.NextProtos = returnedProtocols
		t.Logf("[alpn-server] returned protocol: %v", returnedProtocols)
		return &retCfg, nil
	}
	tlsCfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		var ret tls.Certificate
		for index, cert := range certificates {
			ret.Certificate = append(ret.Certificate, cert.Raw)
			if index == 0 {
				ret.Leaf = cert
			}
		}
		ret.PrivateKey = privateKey
		t.Logf("[alpn-server] returned certificates: %v", ret)
		return &ret, nil
	}

	ln, err := tls.Listen("tcp", host+":0", tlsCfg)
	require.NoError(t, err, "failed to listen with TLS config")

	doOneAccept := func() {
		t.Logf("[alpn-server] starting accept...")
		connRaw, err := ln.Accept()
		require.NoError(t, err, "failed to accept TLS connection")

		t.Logf("[alpn-server] got connection...")
		conn := tls.Server(connRaw.(*tls.Conn), tlsCfg)

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer func() {
			t.Logf("[alpn-server] defer context cancel executing")
			cancel()
		}()

		t.Logf("[alpn-server] starting handshake...")
		if err := conn.HandshakeContext(ctx); err != nil {
			t.Logf("[alpn-server] got non-fatal error while handshaking connection: %v", err)
		}

		t.Logf("[alpn-server] closing connection...")
		if err := conn.Close(); err != nil {
			t.Logf("[alpn-server] got non-fatal error while closing connection: %v", err)
		}
	}

	ALPNPort = strings.Split(ln.Addr().String(), ":")[1]

	type alpnTestCase struct {
		name         string
		certificates []*x509.Certificate
		privateKey   crypto.PrivateKey
		protocols    []string
		token        string
		thumbprint   string
		shouldFail   bool
	}

	var alpnTestCases []alpnTestCase
	// Add all of our keyAuthorizationTestCases into alpnTestCases
	for index, tc := range keyAuthorizationTestCases {
		t.Logf("using keyAuthorizationTestCase [tc=%d] as alpnTestCase [tc=%d]...", index, len(alpnTestCases))
		// Properly encode the authorization.
		checksum := sha256.Sum256([]byte(tc.keyAuthz))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])

		// Build a self-signed certificate.
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating private key")
		tmpl := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: host,
			},
			Issuer: pkix.Name{
				CommonName: host,
			},
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:    key.Public(),
			SerialNumber: big.NewInt(1),
			DNSNames:     []string{host},
			ExtraExtensions: []pkix.Extension{
				{
					Id:       OIDACMEIdentifier,
					Critical: true,
					Value:    []byte(authz),
				},
			},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		require.NoError(t, err, "failed to create certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated certificate")

		newTc := alpnTestCase{
			name:         fmt.Sprintf("keyAuthorizationTestCase[%d]", index),
			certificates: []*x509.Certificate{cert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        tc.token,
			thumbprint:   tc.thumbprint,
			shouldFail:   tc.shouldFail,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: Longer chain
		// Build a self-signed certificate.
		rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating root private key")
		tmpl := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: "Root CA",
			},
			Issuer: pkix.Name{
				CommonName: "Root CA",
			},
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:             rootKey.Public(),
			SerialNumber:          big.NewInt(1),
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		rootCertBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, rootKey.Public(), rootKey)
		require.NoError(t, err, "failed to create root certificate")
		rootCert, err := x509.ParseCertificate(rootCertBytes)
		require.NoError(t, err, "failed to parse newly generated root certificate")

		// Compute our authorization.
		checksum := sha256.Sum256([]byte("valid.valid"))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])

		// Build a leaf certificate which _could_ pass validation
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating leaf private key")
		tmpl = &x509.Certificate{
			Subject: pkix.Name{
				CommonName: host,
			},
			Issuer: pkix.Name{
				CommonName: "Root CA",
			},
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:    key.Public(),
			SerialNumber: big.NewInt(2),
			DNSNames:     []string{host},
			ExtraExtensions: []pkix.Extension{
				{
					Id:       OIDACMEIdentifier,
					Critical: true,
					Value:    []byte(authz),
				},
			},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, rootCert, key.Public(), rootKey)
		require.NoError(t, err, "failed to create leaf certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated leaf certificate")

		newTc := alpnTestCase{
			name:         "longer chain with valid leaf",
			certificates: []*x509.Certificate{cert, rootCert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   true,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: cert without DNSSan
		// Compute our authorization.
		checksum := sha256.Sum256([]byte("valid.valid"))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])

		// Build a leaf certificate without a DNSSan
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating leaf private key")
		tmpl := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: host,
			},
			Issuer: pkix.Name{
				CommonName: host,
			},
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:    key.Public(),
			SerialNumber: big.NewInt(2),
			// NO DNSNames
			ExtraExtensions: []pkix.Extension{
				{
					Id:       OIDACMEIdentifier,
					Critical: true,
					Value:    []byte(authz),
				},
			},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		require.NoError(t, err, "failed to create leaf certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated leaf certificate")

		newTc := alpnTestCase{
			name:         "valid keyauthz without valid dnsname",
			certificates: []*x509.Certificate{cert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   true,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: cert without matching DNSSan
		// Compute our authorization.
		checksum := sha256.Sum256([]byte("valid.valid"))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])

		// Build a leaf certificate which fails validation due to bad DNSName
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating leaf private key")
		tmpl := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: host,
			},
			Issuer: pkix.Name{
				CommonName: host,
			},
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:    key.Public(),
			SerialNumber: big.NewInt(2),
			DNSNames:     []string{host + ".dadgarcorp.com" /* not matching host! */},
			ExtraExtensions: []pkix.Extension{
				{
					Id:       OIDACMEIdentifier,
					Critical: true,
					Value:    []byte(authz),
				},
			},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		require.NoError(t, err, "failed to create leaf certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated leaf certificate")

		newTc := alpnTestCase{
			name:         "valid keyauthz without matching dnsname",
			certificates: []*x509.Certificate{cert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   true,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: cert with additional SAN
		// Compute our authorization.
		checksum := sha256.Sum256([]byte("valid.valid"))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])

		// Build a leaf certificate which has an invalid additional SAN
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating leaf private key")
		tmpl := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: host,
			},
			Issuer: pkix.Name{
				CommonName: host,
			},
			KeyUsage:       x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:      key.Public(),
			SerialNumber:   big.NewInt(2),
			DNSNames:       []string{host},
			EmailAddresses: []string{"webmaster@" + host}, /* unexpected */
			ExtraExtensions: []pkix.Extension{
				{
					Id:       OIDACMEIdentifier,
					Critical: true,
					Value:    []byte(authz),
				},
			},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		require.NoError(t, err, "failed to create leaf certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated leaf certificate")

		newTc := alpnTestCase{
			name:         "valid keyauthz with additional email SANs",
			certificates: []*x509.Certificate{cert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   true,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: cert without CN
		// Compute our authorization.
		checksum := sha256.Sum256([]byte("valid.valid"))
		authz := base64.RawURLEncoding.EncodeToString(checksum[:])

		// Build a leaf certificate which should pass validation
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating leaf private key")
		tmpl := &x509.Certificate{
			Subject:      pkix.Name{},
			Issuer:       pkix.Name{},
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:    key.Public(),
			SerialNumber: big.NewInt(2),
			DNSNames:     []string{host},
			ExtraExtensions: []pkix.Extension{
				{
					Id:       OIDACMEIdentifier,
					Critical: true,
					Value:    []byte(authz),
				},
			},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		require.NoError(t, err, "failed to create leaf certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated leaf certificate")

		newTc := alpnTestCase{
			name:         "valid certificate; no Subject/Issuer (missing CN)",
			certificates: []*x509.Certificate{cert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   false,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: cert without the extension
		// Build a leaf certificate which should fail validation
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating leaf private key")
		tmpl := &x509.Certificate{
			Subject:               pkix.Name{},
			Issuer:                pkix.Name{},
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:             key.Public(),
			SerialNumber:          big.NewInt(1),
			DNSNames:              []string{host},
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		require.NoError(t, err, "failed to create leaf certificate")
		cert, err := x509.ParseCertificate(certBytes)
		require.NoError(t, err, "failed to parse newly generated leaf certificate")

		newTc := alpnTestCase{
			name:         "missing required acmeIdentifier extension",
			certificates: []*x509.Certificate{cert},
			privateKey:   key,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   true,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	{
		// Test case: root without a leaf
		// Build a self-signed certificate.
		rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err, "failed generating root private key")
		tmpl := &x509.Certificate{
			Subject: pkix.Name{
				CommonName: "Root CA",
			},
			Issuer: pkix.Name{
				CommonName: "Root CA",
			},
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			PublicKey:             rootKey.Public(),
			SerialNumber:          big.NewInt(1),
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		rootCertBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, rootKey.Public(), rootKey)
		require.NoError(t, err, "failed to create root certificate")
		rootCert, err := x509.ParseCertificate(rootCertBytes)
		require.NoError(t, err, "failed to parse newly generated root certificate")

		newTc := alpnTestCase{
			name:         "root without leaf",
			certificates: []*x509.Certificate{rootCert},
			privateKey:   rootKey,
			protocols:    []string{ALPNProtocol},
			token:        "valid",
			thumbprint:   "valid",
			shouldFail:   true,
		}
		alpnTestCases = append(alpnTestCases, newTc)
	}

	for index, tc := range alpnTestCases {
		t.Logf("\n\n[tc=%d/name=%s] starting validation", index, tc.name)
		certificates = tc.certificates
		privateKey = tc.privateKey
		returnedProtocols = tc.protocols

		// Attempt to validate the challenge.
		go doOneAccept()
		isValid, err := ValidateTLSALPN01Challenge(host, tc.token, tc.thumbprint, config)
		if !isValid && err == nil {
			t.Fatalf("[tc=%d/name=%s] expected failure to give reason via err (%v / %v)", index, tc.name, isValid, err)
		}

		expectedValid := !tc.shouldFail
		if expectedValid != isValid {
			t.Fatalf("[tc=%d/name=%s] got ret=%v (err=%v), expected ret=%v (shouldFail=%v)", index, tc.name, isValid, err, expectedValid, tc.shouldFail)
		} else if err != nil {
			t.Logf("[tc=%d/name=%s] got expected failure: err=%v", index, tc.name, err)
		}
	}
}
