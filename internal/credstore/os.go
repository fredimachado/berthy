package credstore

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/zalando/go-keyring"
)

// OS returns the operating system's credential store under service.
func OS(service string) Store { return &osStore{service: service} }

type osStore struct {
	service string
}

func (o *osStore) Name() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows Credential Manager"
	case "darwin":
		return "macOS Keychain"
	default:
		return "Secret Service"
	}
}

func (o *osStore) Get(k Key) (string, error) {
	if !sessionBus() {
		return "", ErrUnavailable
	}
	v, err := keyring.Get(o.service, k.account())
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return v, nil
}

func (o *osStore) Set(k Key, secret string) error {
	if !sessionBus() {
		return ErrUnavailable
	}
	err := keyring.Set(o.service, k.account(), secret)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (o *osStore) Delete(k Key) (bool, error) {
	if !sessionBus() {
		return false, ErrUnavailable
	}
	err := keyring.Delete(o.service, k.account())
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return true, nil
}
