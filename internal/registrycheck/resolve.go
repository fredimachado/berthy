package registrycheck

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"
)

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

var sha256Digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ResolveTag returns the registry's canonical sha256 digest for an exact tag.
func (p Probe) ResolveTag(ctx context.Context, repo, tag string) (string, error) {
	if !repoPattern.MatchString(repo) {
		return "", errors.New("invalid repository")
	}
	if !tagPattern.MatchString(tag) {
		return "", errors.New("invalid tag")
	}
	origin, err := listingOrigin(p.Host)
	if err != nil {
		return "", err
	}
	target := *origin
	target.Path = "/v2/" + repo + "/manifests/" + tag
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	flight := newRegistryFlight(ctx, p, origin, repo)
	status, header, _, err := flight.request(http.MethodHead, &target, "", manifestAccept)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		auth, err := flight.authenticate(header)
		if err != nil {
			return "", err
		}
		status, header, _, err = flight.request(http.MethodHead, &target, auth, manifestAccept)
		if err != nil {
			return "", err
		}
	}
	if status == 401 || status == 403 || status == 404 {
		return "", ErrDenied
	}
	if status != 200 {
		return "", errors.New("registry manifest request failed")
	}
	digests := header.Values("Docker-Content-Digest")
	if len(digests) != 1 || !sha256Digest.MatchString(digests[0]) {
		return "", errors.New("registry did not provide one valid sha256 manifest digest")
	}
	return digests[0], nil
}
