//go:build !linux

package metrics

import "context"

type LinuxHost struct{}

func NewHostReader() *LinuxHost { return &LinuxHost{} }
func (*LinuxHost) Read(context.Context) Host {
	return Host{Errors: []string{"host metrics require Linux"}}
}
