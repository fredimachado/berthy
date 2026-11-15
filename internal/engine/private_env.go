package engine

import (
	"context"
	"fmt"
)

// ContainerEnv returns private container configuration; callers must not expose it.
func (e *Engine) ContainerEnv(ctx context.Context, id string) ([]string, error) {
	p, err := pathID(id)
	if err != nil {
		return nil, err
	}
	var data struct{ Config *struct{ Env []string } }
	if err = e.jsonRequest(ctx, "GET", "/containers/"+p+"/json", nil, nil, &data); err != nil {
		return nil, err
	}
	if data.Config == nil {
		return nil, fmt.Errorf("container configuration unavailable")
	}
	return data.Config.Env, nil
}
