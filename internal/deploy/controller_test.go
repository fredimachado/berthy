package deploy

import (
	"berthy/internal/manifest"
	"context"
	"testing"
)

func TestProtectedPlanRejectsTagBeforeEngineAccess(t *testing.T) {
	c := Controller{Protected: true}
	_, err := c.Plan(context.Background(), Actor{}, Request{App: "web", Version: "latest", Manifest: manifest.Manifest{Project: "example", Network: "private", Apps: map[string]manifest.App{"web": {Kind: "image", Image: "example/web", Roles: map[string]manifest.Role{"web": {}}}}}})
	if err == nil {
		t.Fatal("protected tag was accepted")
	}
}
