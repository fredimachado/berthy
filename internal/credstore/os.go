package credstore

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/zalando/go-keyring"
)

// keyringBackend is the OS credential library. The real backend is go-keyring; tests inject a fake.
type keyringBackend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type goKeyring struct{}

func (goKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (goKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (goKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

// OS returns the operating system's credential store under service.
func OS(service string) Store { return &osStore{service: service, ring: goKeyring{}} }

type osStore struct {
	service string
	ring    keyringBackend
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
	v, err := o.ring.Get(o.service, k.Account())
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("%w: get failed", ErrUnavailable)
	}
	return v, nil
}

func (o *osStore) Set(k Key, secret string) error {
	if !sessionBus() {
		return ErrUnavailable
	}
	account := k.Account()
	if err := o.ring.Set(o.service, account, secret); err != nil {
		return fmt.Errorf("%w: save failed", ErrUnavailable)
	}
	// Secret Service can return nil from Set when the unlock prompt is dismissed; nothing was stored.
	got, err := o.ring.Get(o.service, account)
	if err != nil || got != secret {
		return fmt.Errorf("%w: save failed", ErrUnavailable)
	}
	return nil
}

func (o *osStore) Delete(k Key) (bool, error) {
	if !sessionBus() {
		return false, ErrUnavailable
	}
	account := k.Account()
	err := o.ring.Delete(o.service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: delete failed", ErrUnavailable)
	}
	_, err = o.ring.Get(o.service, account)
	if !errors.Is(err, keyring.ErrNotFound) {
		return false, fmt.Errorf("%w: delete failed", ErrUnavailable)
	}
	return true, nil
}
