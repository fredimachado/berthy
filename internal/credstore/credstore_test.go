package credstore

import (
	"errors"
	"testing"
)

func TestKeyAccountNamesTheEnvironmentTheAgentAndTheVariable(t *testing.T) {
	base := Key{
		URL:         "http://10.0.0.5:7710/",
		Project:     "todo",
		Environment: "dev",
		Variable:    "BERTHY_TODO_DEV_TOKEN",
	}
	want := "todo/dev@http://10.0.0.5:7710#BERTHY_TODO_DEV_TOKEN"
	if got := base.Account(); got != want {
		t.Fatalf("Account = %q, want %q", got, want)
	}
	other := base
	other.Variable = "BERTHY_TODO_DEV_SCRIPT_TOKEN"
	if other.Account() == base.Account() {
		t.Fatalf("keys that differ only in Variable share Account %q", base.Account())
	}
}

func TestNoneIsUnavailable(t *testing.T) {
	s := None()
	if s.Name() != "credential store" {
		t.Fatalf("Name = %q", s.Name())
	}
	key := Key{URL: "http://10.0.0.5:7710", Project: "todo", Environment: "dev", Variable: "BERTHY_TODO_DEV_TOKEN"}
	if _, err := s.Get(key); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get: %v", err)
	}
	if err := s.Set(key, "secret"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set: %v", err)
	}
	if removed, err := s.Delete(key); removed || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete: removed=%v err=%v", removed, err)
	}
}

func TestFromEnvironment(t *testing.T) {
	s, err := FromEnvironment(func(string) string { return "" })
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if got, want := s.Name(), OS(Service).Name(); got != want {
		t.Fatalf("empty: Name = %q, want %q", got, want)
	}
	s, err = FromEnvironment(func(string) string { return "none" })
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	if s.Name() != "credential store" {
		t.Fatalf("none: Name = %q", s.Name())
	}
	if _, err := FromEnvironment(func(string) string { return "nonee" }); err == nil {
		t.Fatal("nonee: want an error")
	}
}
