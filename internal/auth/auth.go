// Package auth checks the JSON Web Token the host application hands to the
// widget.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalid reports a token that is malformed, signed with another key or
// missing a required claim.
var ErrInvalid = errors.New("token is not valid")

// ErrExpired reports a token whose expiry has passed.
var ErrExpired = errors.New("token is expired")

// Claims are the values a checked token carries.
type Claims struct {
	// Sub is the user ID.
	Sub string
	// Name is the display name. It may be empty.
	Name string
	// Exp is the point in time the token stops being valid.
	Exp time.Time
}

// header is the part of a token header that matters.
type header struct {
	// Alg is the signing algorithm. Only HS256 is accepted.
	Alg string `json:"alg"`
}

// payload is the part of a token payload that matters.
type payload struct {
	// Sub is the user ID.
	Sub string `json:"sub"`
	// Name is the display name.
	Name string `json:"name"`
	// Exp is the expiry as Unix seconds. It may be written as a decimal.
	Exp *json.Number `json:"exp"`
}

// Verify checks the signature and the claims of token against secret and
// reports the claims it carries. It returns ErrExpired for a token that is no
// longer valid at now, and ErrInvalid for every other rejection.
func Verify(token string, secret []byte, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: it does not have three parts", ErrInvalid)
	}

	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: the header is not base64", ErrInvalid)
	}
	var head header
	if err := json.Unmarshal(rawHeader, &head); err != nil {
		return Claims{}, fmt.Errorf("%w: the header is not JSON", ErrInvalid)
	}
	if head.Alg != "HS256" {
		return Claims{}, fmt.Errorf("%w: the algorithm is %q, not HS256", ErrInvalid, head.Alg)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: the signature is not base64", ErrInvalid)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Claims{}, fmt.Errorf("%w: the signature does not match", ErrInvalid)
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: the payload is not base64", ErrInvalid)
	}
	var body payload
	if err := json.Unmarshal(rawPayload, &body); err != nil {
		return Claims{}, fmt.Errorf("%w: the payload is not JSON", ErrInvalid)
	}
	if body.Sub == "" {
		return Claims{}, fmt.Errorf("%w: the sub claim is missing", ErrInvalid)
	}
	if body.Exp == nil {
		return Claims{}, fmt.Errorf("%w: the exp claim is missing", ErrInvalid)
	}
	seconds, err := body.Exp.Float64()
	if err != nil {
		return Claims{}, fmt.Errorf("%w: the exp claim is not a number", ErrInvalid)
	}

	claims := Claims{
		Sub:  body.Sub,
		Name: body.Name,
		Exp:  time.Unix(int64(seconds), 0),
	}
	if !claims.Exp.After(now) {
		return Claims{}, fmt.Errorf("%w: it ran out at %s", ErrExpired, claims.Exp.UTC().Format(time.RFC3339))
	}
	return claims, nil
}

// signHeader is the fixed header of a signed token.
const signHeader = `{"alg":"HS256","typ":"JWT"}`

// signPayload is the payload Sign writes. A token without a display name
// carries no name claim.
type signPayload struct {
	// Sub is the user ID.
	Sub string `json:"sub"`
	// Name is the display name.
	Name string `json:"name,omitempty"`
	// Exp is the expiry as Unix seconds.
	Exp int64 `json:"exp"`
}

// Sign builds a token that carries claims and signs it with secret.
func Sign(claims Claims, secret []byte) string {
	body, _ := json.Marshal(signPayload{
		Sub:  claims.Sub,
		Name: claims.Name,
		Exp:  claims.Exp.Unix(),
	})
	signed := base64.RawURLEncoding.EncodeToString([]byte(signHeader)) + "." +
		base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
