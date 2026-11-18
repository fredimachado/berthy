package deploy

import "testing"

// The connector's release carries a manifest that Berthy builds, so an ingress apply never writes the environment's settings.
func TestAnIngressApplyLeavesTheSettingsAlone(t *testing.T) {
	f := newIngressFixture(t)
	m := withIngress(t, removeApps("api.example.com"))
	f.applyIngress("apply", m)
	if _, ok, err := f.store.Settings(t.Context()); ok || err != nil {
		t.Fatalf("settings after an ingress apply: %v %v", ok, err)
	}
	if op := f.deploy("deploy-api", "api", apiFirst, m); op.State != "succeeded" {
		t.Fatalf("deploy api %s: %s", op.State, op.Err)
	}
	st, ok, err := f.store.Settings(t.Context())
	if err != nil || !ok || st.Project != "demo" || len(st.Apps) != 1 || st.Apps[0] != "api" {
		t.Fatalf("settings after the deploy: %+v %v %v", st, ok, err)
	}
	f.applyIngress("apply-again", m)
	if again, _, _ := f.store.Settings(t.Context()); again.OperationID != st.OperationID {
		t.Fatalf("an ingress apply rewrote the settings: %+v", again)
	}
}
