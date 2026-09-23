// Package secrets abstracts credential storage. The system implementation uses
// the freedesktop Secret Service (org.freedesktop.secrets) over D-Bus via
// zalando/go-keyring; an in-memory implementation is provided for tests.
package secrets

import (
	"errors"

	"github.com/zalando/go-keyring"
)

// ErrNotFound is returned when a key is absent.
var ErrNotFound = errors.New("secret not found")

// service is the keyring collection label for all mmcli secrets.
const service = "mmcli"

// Store is the minimal credential store interface used by the rest of mmcli.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// SystemStore talks to the OS keyring (Secret Service on Linux).
type SystemStore struct{}

// Get returns the value for key, or ErrNotFound.
func (SystemStore) Get(key string) (string, error) {
	v, err := keyring.Get(service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// Set stores value under key.
func (SystemStore) Set(key, value string) error {
	return keyring.Set(service, key, value)
}

// Delete removes key; absent keys are not an error.
func (SystemStore) Delete(key string) error {
	err := keyring.Delete(service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// Key helpers keep the keyring namespace consistent across the codebase.

// PasswordKey is the keyring key holding a context's login password.
func PasswordKey(ctx string) string { return "password:" + ctx }

// TokenKey is the keyring key holding a context's cached session token.
func TokenKey(ctx string) string { return "token:" + ctx }
