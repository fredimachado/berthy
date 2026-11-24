package sshhost

import "testing"

func TestValid(t *testing.T) {
	good := []string{"s1", "ops@s1", "deploy_user@10.0.0.5", "root@db-1.example.com", "a.b", "_svc@host"}
	bad := []string{"", "-oProxyCommand=x", "-s1", "ops@-s1", "ops@", "@s1", "Ops@s1", "ops@s1;id", "ops@s1 id", "ops@@s1", "ops@s1\n", "ops@[::1]", "-ops@s1", "ops@s1:22", "a b"}
	for _, h := range good {
		if !Valid(h) {
			t.Errorf("Valid(%q) = false", h)
		}
	}
	for _, h := range bad {
		if Valid(h) {
			t.Errorf("Valid(%q) = true", h)
		}
	}
}
