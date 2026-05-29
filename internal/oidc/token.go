package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	signingAlgorithm = jose.RS256
	rsaKeyBits       = 2048

	// DefaultTokenTTL is the default lifetime for issued access tokens.
	DefaultTokenTTL = time.Hour

	// keyFileName is the name of the persisted signing key inside the OIDC state dir.
	signingKeyFileName = "signing-key.pem"
)

// SigningKey holds the RSA key used to sign issued JWTs along with its JWKS-friendly key ID.
type SigningKey struct {
	Key   *rsa.PrivateKey
	KeyID string
}

// LoadOrCreateSigningKey loads the signing key from dir/signing-key.pem, generating
// a fresh RSA-2048 key if one does not exist. The key ID is the SHA-256 fingerprint
// of the DER-encoded public key, truncated to 16 hex chars.
func LoadOrCreateSigningKey(dir string) (*SigningKey, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create oidc state dir: %w", err)
	}

	path := filepath.Join(dir, signingKeyFileName)
	if data, err := os.ReadFile(path); err == nil {
		key, kerr := parseRSAPrivateKeyPEM(data)
		if kerr == nil {
			return &SigningKey{Key: key, KeyID: keyIDFor(&key.PublicKey)}, nil
		}
		// Fall through to regenerate if the persisted key is unreadable.
	}

	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate rsa key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal rsa key: %w", err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write rsa key: %w", err)
	}
	return &SigningKey{Key: key, KeyID: keyIDFor(&key.PublicKey)}, nil
}

// Claims is the JWT claim set issued by bladerunner's local OIDC provider.
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	Expiry    int64  `json:"exp"`
	ClientID  string `json:"client_id,omitempty"`
	// Fingerprint is the SHA-256 fingerprint of the SSH key used to authenticate.
	// It is also the Subject; we duplicate it as an explicit claim so consumers
	// can introspect without parsing the sub format.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Comment is the SSH key comment (often user@host) for human-friendly display.
	Comment string `json:"comment,omitempty"`
}

// Issuer signs tokens for registered identities.
type Issuer struct {
	signer   jose.Signer
	key      *SigningKey
	issuer   string
	audience string
	ttl      time.Duration
}

// NewIssuer creates a JWT issuer that signs tokens with the given key.
// issuer is the OIDC `iss` claim (e.g. "http://127.0.0.1:15556").
// audience is the expected Incus audience (e.g. "bladerunner").
func NewIssuer(key *SigningKey, issuer, audience string, ttl time.Duration) (*Issuer, error) {
	if key == nil || key.Key == nil {
		return nil, errors.New("nil signing key")
	}
	signerKey := jose.SigningKey{Algorithm: signingAlgorithm, Key: key.Key}
	opts := (&jose.SignerOptions{}).WithType("JWT")
	opts.WithHeader(jose.HeaderKey("kid"), key.KeyID)
	signer, err := jose.NewSigner(signerKey, opts)
	if err != nil {
		return nil, fmt.Errorf("new signer: %w", err)
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return &Issuer{
		signer:   signer,
		key:      key,
		issuer:   issuer,
		audience: audience,
		ttl:      ttl,
	}, nil
}

// Issue mints a signed JWT for the given identity. The token's `sub` claim is
// the identity's SHA-256 fingerprint.
func (i *Issuer) Issue(ident Identity, clientID string) (string, *Claims, error) {
	if ident.Fingerprint == "" {
		return "", nil, errors.New("identity has no fingerprint")
	}
	now := time.Now().UTC()
	claims := Claims{
		Issuer:      i.issuer,
		Subject:     ident.Fingerprint,
		Audience:    i.audience,
		IssuedAt:    now.Unix(),
		NotBefore:   now.Unix(),
		Expiry:      now.Add(i.ttl).Unix(),
		ClientID:    clientID,
		Fingerprint: ident.Fingerprint,
		Comment:     ident.Comment,
	}
	tok, err := jwt.Signed(i.signer).Claims(claims).Serialize()
	if err != nil {
		return "", nil, fmt.Errorf("sign token: %w", err)
	}
	return tok, &claims, nil
}

// Verify parses a signed JWT and validates it against the issuer's expected
// issuer and audience. It does NOT check whether the token's subject is still
// a registered identity — callers should re-check the store for revocation.
func (i *Issuer) Verify(tokenString string) (*Claims, error) {
	parsed, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{signingAlgorithm})
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	var c Claims
	if err := parsed.Claims(&i.key.Key.PublicKey, &c); err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}
	now := time.Now().Unix()
	if c.Issuer != i.issuer {
		return nil, fmt.Errorf("unexpected issuer: %s", c.Issuer)
	}
	if c.Audience != i.audience {
		return nil, fmt.Errorf("unexpected audience: %s", c.Audience)
	}
	if c.Expiry != 0 && now >= c.Expiry {
		return nil, errors.New("token expired")
	}
	if c.NotBefore != 0 && now+1 < c.NotBefore {
		return nil, errors.New("token not yet valid")
	}
	return &c, nil
}

// JWKS returns the JSON Web Key Set advertising the issuer's public key.
func (i *Issuer) JWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       &i.key.Key.PublicKey,
				KeyID:     i.key.KeyID,
				Algorithm: string(signingAlgorithm),
				Use:       "sig",
			},
		},
	}
}

// JWKSJSON returns the JWKS marshaled to JSON.
func (i *Issuer) JWKSJSON() ([]byte, error) {
	set := i.JWKS()
	return json.Marshal(set)
}

// Issuer returns the issuer URL used in tokens.
func (i *Issuer) Issuer() string { return i.issuer }

// Audience returns the audience value used in tokens.
func (i *Issuer) Audience() string { return i.audience }

// parseRSAPrivateKeyPEM decodes a PKCS#8-encoded RSA private key.
func parseRSAPrivateKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return key, nil
}

// keyIDFor returns a short, stable identifier for an RSA public key.
func keyIDFor(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "key0"
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}
