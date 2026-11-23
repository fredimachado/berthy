//go:build !linux

package credstore

func sessionBus() bool { return true }
