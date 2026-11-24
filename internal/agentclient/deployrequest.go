package agentclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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

// CheckManifestProject refuses a deploy whose manifest is for a different project than the target. The text is the same for
// the CLI and for MCP, so a mismatch is refused before any request in both.
func CheckManifestProject(dir, have, want string) error {
	if have == want {
		return nil
	}
	return fmt.Errorf("the manifest in %s is for project %s, not %s", dir, have, want)
}

// PrepareDeploy fills in what a deploy request carries from the manifest directory: the merged manifest for env and, for a
// Compose app, its Compose file. req.App and req.Version are set. It refuses an image app without a version (unless it keeps
// the version it runs, req.KeepVersion, which the agent resolves), a Compose app with one, and an image that the manifest
// does not allow for the app. Then it stamps the request from the checkout (see StampCheckout) and returns what git said.
func PrepareDeploy(dir, env string, req *deploy.Request) (gitcheck.Checkout, error) {
	m, err := manifest.Load(dir, env)
	if err != nil {
		return gitcheck.Checkout{}, err
	}
	if m.Apps[req.App].Kind == "compose" {
		if req.Version != "" {
			return gitcheck.Checkout{}, ErrVersionNotAllowed
		}
	} else if err := checkImageVersion(m, req); err != nil {
		return gitcheck.Checkout{}, err
	}
	return PrepareDeployFiles(context.Background(), dir, env, m, []*deploy.Request{req})
}

// inspectDeployFiles permits a deterministic checkout switch during status in tests.
var inspectDeployFiles = gitcheck.Inspect

// PrepareDeployFiles freezes the manifest, selected Compose files and checkout facts for a batch.
func PrepareDeployFiles(ctx context.Context, dir, env string, m *manifest.Manifest, reqs []*deploy.Request) (gitcheck.Checkout, error) {
	files := append([]string{}, manifest.Files(env)...)
	paths := make(map[*deploy.Request]string)
	for _, req := range reqs {
		req.Manifest = *m
		if app := m.Apps[req.App]; app.Kind == "compose" {
			file, err := ReadComposeFile(dir, app.File)
			if err != nil {
				return gitcheck.Checkout{}, err
			}
			req.ComposeFile = file
			paths[req] = path.Clean(filepath.ToSlash(manifest.FilePath(app.File)))
			files = append(files, manifest.FilePath(app.File))
		}
	}
	co, err := inspectDeployFiles(ctx, dir, files)
	if err != nil {
		return co, err
	}
	for _, req := range reqs {
		if req.Commit == "" {
			req.Commit = co.Head
		}
		req.Modified = false
		for _, changed := range co.Changed {
			for _, file := range manifest.Files(env) {
				if changed == filepath.ToSlash(file) {
					req.Modified = true
				}
			}
			if paths[req] != "" && changed == paths[req] {
				req.Modified = true
			}
		}
	}
	return co, nil
}

// PrepareDeployFilesChecked completes a capture begun before manifest.Load. Callers must pass the
// identity from before reading any manifest or Compose input; changed checkout identity refuses the request.
// Discard the prepared requests if this returns an error.
func PrepareDeployFilesChecked(ctx context.Context, dir, env string, m *manifest.Manifest, reqs []*deploy.Request, before gitcheck.Identity) (gitcheck.Checkout, error) {
	co, err := PrepareDeployFiles(ctx, dir, env, m, reqs)
	if err != nil {
		return co, err
	}
	after, err := gitcheck.Capture(ctx, dir)
	if err != nil {
		return co, err
	}
	if !before.Same(after) || co.InWorkTree != before.InWorkTree || co.Head != before.Head {
		return co, errors.New("checkout changed while reading deploy files; retry the deploy")
	}
	return co, nil
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
	path := filepath.Join(root, manifest.FilePath(file))
	if !inside(path) {
		return "", outside
	}
	resolved, err := filepath.EvalSymlinks(path)
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
