package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// secret is the shared key used by the tests.
var secret = []byte("shared key")

// makeToken builds a token from the given header and payload JSON and signs it
// with key.
func makeToken(t *testing.T, header, payload string, key []byte) string {
	t.Helper()
	encode := base64.RawURLEncoding.EncodeToString
	signed := encode([]byte(header)) + "." + encode([]byte(payload))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	return signed + "." + encode(mac.Sum(nil))
}

// TestVerifyValid checks that a well formed token returns its claims.
func TestVerifyValid(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	token := makeToken(t, `{"alg":"HS256","typ":"JWT"}`, `{"sub":"u1","name":"Anna","exp":1000060}`, secret)

	claims, err := Verify(token, secret, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Sub != "u1" || claims.Name != "Anna" {
		t.Errorf("claims = %+v", claims)
	}
	if !claims.Exp.Equal(time.Unix(1000060, 0)) {
		t.Errorf("exp = %s", claims.Exp)
	}
}

// TestVerifyFloatExpiry checks that a decimal exp claim is cut to whole seconds.
func TestVerifyFloatExpiry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	token := makeToken(t, `{"alg":"HS256"}`, `{"sub":"u1","exp":1000060.5}`, secret)

	claims, err := Verify(token, secret, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !claims.Exp.Equal(time.Unix(1000060, 0)) {
		t.Errorf("exp = %s", claims.Exp)
	}
}

// TestVerifyRejects checks the error reported for every kind of bad token.
func TestVerifyRejects(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name  string
		token string
		want  error
	}{
		{
			name:  "wrong signature",
			token: makeToken(t, `{"alg":"HS256"}`, `{"sub":"u1","exp":1000060}`, []byte("other key")),
			want:  ErrInvalid,
		},
		{
			name:  "expired",
			token: makeToken(t, `{"alg":"HS256"}`, `{"sub":"u1","exp":999999}`, secret),
			want:  ErrExpired,
		},
		{
			name:  "expiry is now",
			token: makeToken(t, `{"alg":"HS256"}`, `{"sub":"u1","exp":1000000}`, secret),
			want:  ErrExpired,
		},
		{
			name:  "algorithm none",
			token: makeToken(t, `{"alg":"none"}`, `{"sub":"u1","exp":1000060}`, secret),
			want:  ErrInvalid,
		},
		{
			name:  "missing sub",
			token: makeToken(t, `{"alg":"HS256"}`, `{"exp":1000060}`, secret),
			want:  ErrInvalid,
		},
		{
			name:  "missing exp",
			token: makeToken(t, `{"alg":"HS256"}`, `{"sub":"u1"}`, secret),
			want:  ErrInvalid,
		},
		{
			name:  "two parts",
			token: strings.Join(strings.Split(makeToken(t, `{"alg":"HS256"}`, `{"sub":"u1","exp":1000060}`, secret), ".")[:2], "."),
			want:  ErrInvalid,
		},
		{
			name:  "empty",
			token: "",
			want:  ErrInvalid,
		},
		{
			name:  "not base64",
			token: "a!b.c!d.e!f",
			want:  ErrInvalid,
		},
		{
			name:  "header not JSON",
			token: makeToken(t, `no json`, `{"sub":"u1","exp":1000060}`, secret),
			want:  ErrInvalid,
		},
		{
			name:  "payload not JSON",
			token: makeToken(t, `{"alg":"HS256"}`, `no json`, secret),
			want:  ErrInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Verify(c.token, secret, now)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}
