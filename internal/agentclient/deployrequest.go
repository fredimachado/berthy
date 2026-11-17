package agentclient

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"berthy/internal/deploy"
	"berthy/internal/gitcheck"
	"berthy/internal/manifest"
)

// UsageError is a deploy request that the caller got wrong, as opposed to a manifest or file that cannot be used.
type UsageError string

func (e UsageError) Error() string { return string(e) }

// The usage errors of PrepareDeploy. Each caller words them for its own users (flags for the CLI, fields for MCP).
var (
	ErrVersionRequired   = UsageError("an image app needs a version")
	ErrVersionNotAllowed = UsageError("a compose app takes no version")
)

// PrepareDeploy fills in what a deploy request carries from the manifest directory: the merged manifest for env and, for a
// Compose app, its Compose file. req.App and req.Version are set. It refuses an image app without a version (unless it keeps
// the version it runs, req.KeepVersion, which the agent resolves), a Compose app with one, and an image that the manifest
// does not allow for the app. Then it stamps the request from the checkout (see StampCheckout) and returns what git said.
func PrepareDeploy(dir, env string, req *deploy.Request) (gitcheck.Checkout, error) {
	m, err := manifest.Load(dir, env)
	if err != nil {
		return gitcheck.Checkout{}, err
	}
	req.Manifest = *m
	var extra []string
	if m.Apps[req.App].Kind == "compose" {
		if req.Version != "" {
			return gitcheck.Checkout{}, ErrVersionNotAllowed
		}
		file, err := ReadComposeFile(dir, m.Apps[req.App].File)
		if err != nil {
			return gitcheck.Checkout{}, err
		}
		req.ComposeFile = file
		extra = append(extra, m.Apps[req.App].File)
	} else if err := checkImageVersion(m, req); err != nil {
		return gitcheck.Checkout{}, err
	}
	return StampCheckout(dir, env, req, extra...)
}

// checkImageVersion refuses an image app's deploy without a version, or with one its manifest does not allow.
func checkImageVersion(m *manifest.Manifest, req *deploy.Request) error {
	if req.Version == "" {
		if req.KeepVersion {
			// The agent resolves the version from the app's applied release.
			return nil
		}
		return ErrVersionRequired
	}
	_, err := m.CheckImageDeploy(req.App, req.Version, false)
	return err
}

// StampCheckout asks git about the manifest files for env in dir, and about extra (the files of dir the request reads
// besides them), and sets the request's commit and modified flag from the answer. The commit is HEAD unless the request
// already names one. Outside a work tree, or with git missing, the request keeps no commit and is not modified.
func StampCheckout(dir, env string, req *deploy.Request, extra ...string) (gitcheck.Checkout, error) {
	files := append(manifest.Files(env), extra...)
	co, err := gitcheck.Inspect(context.Background(), dir, files)
	if err != nil {
		return co, err
	}
	if req.Commit == "" {
		req.Commit = co.Head
	}
	req.Modified = len(co.Changed) > 0
	return co, nil
}

// ReadComposeFile reads the Compose file of a Compose app from the manifest directory. The file must resolve to a regular
// file inside that directory, and no larger than the Compose file limit.
func ReadComposeFile(dir, file string) (string, error) {
	outside := errors.New("compose file must be inside the manifest directory")
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", outside
	}
	if resolved, e := filepath.EvalSymlinks(root); e == nil {
		root = resolved
	}
	inside := func(p string) bool {
		rel, e := filepath.Rel(root, p)
		return e == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	if !inside(filepath.Join(root, file)) {
		return "", outside
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, file))
	if err != nil || !inside(resolved) {
		return "", outside
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("compose file unavailable")
	}
	if info.Size() > manifest.MaxComposeFile {
		return "", errors.New("compose file exceeds 512 KiB")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", errors.New("compose file unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, manifest.MaxComposeFile+1))
	if err != nil || manifest.CheckComposeFile(b) != nil {
		return "", manifest.ErrComposeFile
	}
	return string(b), nil
}
