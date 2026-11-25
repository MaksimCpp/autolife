package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const passwordParams = "m=19456,t=2,p=1"

var passwordSlots = make(chan struct{}, 4)

func normalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(email)
	if err != nil || len(email) > 254 || address.Address != email || !strings.Contains(email, "@") {
		return "", errors.New("invalid email")
	}
	for _, r := range email {
		if r < 33 || r > 126 {
			return "", errors.New("invalid email")
		}
	}
	return email, nil
}

func validPassword(password string) bool {
	return len(password) <= 256 && utf8.RuneCountInString(password) >= 12
}

func derivePassword(ctx context.Context, password string, salt []byte) ([]byte, error) {
	select {
	case passwordSlots <- struct{}{}:
		defer func() { <-passwordSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32), nil
}

func hashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derivePassword(ctx, password, salt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=19$%s$%s$%s", passwordParams,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func passwordMatches(ctx context.Context, password, encoded string) (bool, error) {
	pieces := strings.Split(encoded, "$")
	if len(pieces) != 6 || pieces[0] != "" || pieces[1] != "argon2id" || pieces[2] != "v=19" || pieces[3] != passwordParams {
		return false, errors.New("invalid password verifier")
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(pieces[4])
	expected, keyErr := base64.RawStdEncoding.DecodeString(pieces[5])
	if saltErr != nil || keyErr != nil || len(salt) != 16 || len(expected) != 32 {
		return false, errors.New("invalid password verifier")
	}
	actual, err := derivePassword(ctx, password, salt)
	return subtle.ConstantTimeCompare(actual, expected) == 1, err
}

func newToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func validToken(token string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(decoded) == 32
}

func hashToken(value string) []byte {
	hash := sha256.Sum256([]byte(value))
	return hash[:]
}
