package setup

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"berthy/internal/version"
)

// DefaultReleaseURL is where releases are downloaded from. A release has berthy-linux-amd64, berthy-linux-arm64 and
// checksums.txt (in sha256sum format) under <base>/<version>/.
const DefaultReleaseURL = "https://github.com/fredimachado/berthy/releases/download"

var versionTag = regexp.MustCompile(`^v[0-9][0-9A-Za-z.+_-]*$`)

// ReleaseBase checks the base URL of releases: raw is BERTHY_RELEASE_URL, or empty for the default. It accepts https, and http
// only on a loopback or private address, so a checksum file is never fetched in the clear from the internet. The error never
// repeats the value.
func ReleaseBase(raw string) (string, error) {
	if raw == "" {
		return DefaultReleaseURL, nil
	}
	bad := errors.New("BERTHY_RELEASE_URL must be an https URL, or an http URL on a loopback or private address")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", bad
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !(ip.IsLoopback() || ip.IsPrivate())) {
			return "", bad
		}
	default:
		return "", bad
	}
	return strings.TrimRight(raw, "/"), nil
}

// binarySource is where the agent binary comes from.
type binarySource struct {
	local   string // the file to upload; empty for a release the server downloads
	sha     string // the SHA-256 of local
	version string // the release to download, or the version of this CLI when it uploads itself
	desc    string // for the output: "v1.2.0", "this CLI (dev)", "--binary agent-linux"
	url     string // the release's directory
}

// checkELF checks that path is an executable ELF file for the server's architecture and returns its SHA-256.
func checkELF(path, arch string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read --binary")
	}
	defer f.Close()
	e, err := elf.NewFile(f)
	if err != nil {
		return "", fmt.Errorf("%s is not a Linux executable", filepath.Base(path))
	}
	defer e.Close()
	if e.Type != elf.ET_EXEC && e.Type != elf.ET_DYN {
		return "", fmt.Errorf("%s is not a Linux executable", filepath.Base(path))
	}
	var built string
	switch e.Machine {
	case elf.EM_X86_64:
		built = "amd64"
	case elf.EM_AARCH64:
		built = "arm64"
	default:
		built = e.Machine.String()
	}
	if built != arch {
		return "", fmt.Errorf("%s is built for %s, but the server is %s", filepath.Base(path), built, arch)
	}
	return fileSHA(path)
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// source picks the binary to install, in this order: --binary, this CLI when it is a Linux build of the server's
// architecture, the release of this CLI's version, and otherwise nothing.
func (s *Session) source(arch string) (*binarySource, error) {
	o := s.Opts
	switch {
	case o.Binary != "":
		sha, err := checkELF(o.Binary, arch)
		if err != nil {
			return nil, err
		}
		return &binarySource{local: o.Binary, sha: sha, desc: "--binary " + filepath.Base(o.Binary)}, nil
	case o.Build.OS == "linux" && o.Build.Arch == arch && o.Build.Path != "":
		sha, err := fileSHA(o.Build.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot read this CLI to upload it: %w", err)
		}
		return &binarySource{local: o.Build.Path, sha: sha, version: o.Build.Version, desc: "this CLI (" + o.Build.Version + ")"}, nil
	case o.Build.Release:
		if !versionTag.MatchString(o.Build.Version) {
			return nil, errors.New("this CLI has no usable release version: pass --binary with a Linux " + arch + " build of Berthy")
		}
		base, err := ReleaseBase(o.ReleaseURL)
		if err != nil {
			return nil, err
		}
		return &binarySource{version: o.Build.Version, desc: o.Build.Version, url: base + "/" + o.Build.Version}, nil
	}
	return nil, fmt.Errorf("this CLI is a development build for %s/%s: pass --binary with a Linux %s build of Berthy",
		orUnknown(o.Build.OS), orUnknown(o.Build.Arch), arch)
}

// serverVersion is the version the agent binary on the server reports, or "" when a completed probe reports none
// (non-zero exit other than 255, or unparsable output). A transport failure from Runner.Run, or exit 255 (a dropped
// OpenSSH connection reported as Result.Code with a nil error), is returned and not cached, so a later connection
// cannot replace a newer agent after a failed probe.
func (s *Session) serverVersion(ctx context.Context) (string, error) {
	if s.serverVer != nil {
		return *s.serverVer, nil
	}
	res, err := s.R.Run(ctx, binPath+" version 2>/dev/null", nil)
	if err != nil {
		return "", fmt.Errorf("reading the agent version: %w", err)
	}
	if res.Code == 255 {
		return "", errors.New("reading the agent version: ssh exited 255")
	}
	v := ""
	if res.Code == 0 {
		// "berthy v1.2.0 linux/amd64"
		if fields := strings.Fields(string(res.Stdout)); len(fields) >= 2 && fields[0] == "berthy" {
			v = clean(fields[1])
		}
	}
	s.serverVer = &v
	return v, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// binaryStep puts the agent binary this CLI wants next to the live one, as berthy.new, and checks its SHA-256. The service
// step swaps it in. A server that already has that binary is left alone.
func binaryStep(ctx context.Context, s *Session) (string, string, error) {
	f, err := s.facts(ctx)
	if err != nil {
		return "", "", err
	}
	arch := goArch(f.Arch)
	src, err := s.source(arch)
	if err != nil {
		return "", "", err
	}
	if f.BinSHA != "" {
		same := false
		if src.local != "" {
			same = f.BinSHA == src.sha
		} else {
			server, err := s.serverVersion(ctx)
			if err != nil {
				return "", "", err
			}
			same = server == src.version
		}
		if same {
			return StatusOK, installedText(src), nil
		}
		// --binary has no version and stays the explicit override; a non-release
		// (dev) version on either side leaves Compare ok=false and skips this.
		if src.version != "" {
			server, err := s.serverVersion(ctx)
			if err != nil {
				return "", "", err
			}
			if c, ok := version.Compare(server, src.version); ok && c > 0 {
				return "", "", fmt.Errorf("the server runs berthy %s, newer than this CLI (%s): upgrade this CLI, or pass --binary to install another build", server, src.version)
			}
		}
	}
	verb := "install"
	if f.BinSHA != "" {
		verb = "replace the agent binary with"
	}
	script, cmdErr := binaryScript(f, src, arch)
	if s.DryRun {
		s.staged.Binary = true
		if cmdErr == nil {
			s.Log.Would(script)
		}
		return StatusWouldChange, "would " + verb + " " + src.desc, nil
	}
	if cmdErr != nil {
		return "", "", cmdErr
	}
	var detail string
	if src.local != "" {
		if err := uploadBinary(ctx, s, src, script); err != nil {
			return "", "", err
		}
		detail = "uploaded " + src.desc + ", sha256 " + shortSHA(src.sha)
	} else {
		if err := downloadBinary(ctx, s, src, script); err != nil {
			return "", "", err
		}
		detail = "downloaded " + src.desc + " and verified its checksum"
	}
	s.staged.Binary = true
	return StatusChanged, detail, nil
}

func installedText(src *binarySource) string {
	if src.local != "" && src.version == "" {
		return src.desc + ", sha256 " + shortSHA(src.sha)
	}
	return src.desc + " is installed"
}

// uploadScript installs the binary that arrives on the script's standard input as berthy.new, and prints its SHA-256.
func uploadScript() string {
	return "set -e\numask 022\n" +
		"cat > " + binPath + ".new\n" +
		"chmod 0755 " + binPath + ".new\n" +
		"sha256sum " + binPath + ".new\n"
}

// fetchTool is the command the server downloads a release with: curl, or else wget.
func fetchTool(f *Facts) (string, error) {
	switch {
	case f.Curl:
		return "curl", nil
	case f.Wget:
		return "wget", nil
	}
	return "", errors.New("neither curl nor wget is available on the server to download the release: pass --binary")
}

// binaryScript is the script that puts the agent binary of src next to the live one: the upload, or the download of a release.
// It fails when the server cannot download a release.
func binaryScript(f *Facts, src *binarySource, arch string) (string, error) {
	if src.local != "" {
		return uploadScript(), nil
	}
	fetch, err := fetchTool(f)
	if err != nil {
		return "", err
	}
	return downloadScript(src.url, "berthy-linux-"+arch, fetch), nil
}

// uploadBinary streams the local file through the runner's standard input into berthy.new, and compares its SHA-256 with the
// local one.
func uploadBinary(ctx context.Context, s *Session, src *binarySource, script string) error {
	file, err := os.Open(src.local)
	if err != nil {
		return errors.New("cannot read the binary to upload")
	}
	defer file.Close()
	res, err := s.R.Run(ctx, script, file)
	if err != nil {
		return fmt.Errorf("uploading the binary: %w", err)
	}
	if res.Code != 0 {
		return fmt.Errorf("uploading the binary: %s", failureText(res))
	}
	if firstField(res.Stdout) != src.sha {
		_, _ = s.R.Run(ctx, "rm -f "+binPath+".new", nil)
		return errors.New("the uploaded binary does not match the local file (SHA-256 differs)")
	}
	return nil
}

// downloadScript downloads a release next to the live binary, checks it against the release's checksums.txt, and leaves it as
// berthy.new. The download goes to a directory of /usr/local/bin, so the final move is a rename on one filesystem.
func downloadScript(base, asset, fetch string) string {
	get := func(u, dest string) string {
		if fetch == "wget" {
			return "wget -qO " + dest + " " + shellQuote(u)
		}
		return "curl -fsSL " + shellQuote(u) + " -o " + dest
	}
	return "set -e\numask 022\n" +
		"dir=$(dirname " + binPath + ")\n" +
		"tmp=$(mktemp -d \"$dir/.berthy-download.XXXXXX\")\n" +
		"trap 'rm -rf \"$tmp\"' EXIT\n" +
		get(base+"/"+asset, "\"$tmp/"+asset+"\"") + "\n" +
		get(base+"/checksums.txt", "\"$tmp/checksums.txt\"") + "\n" +
		"cd \"$tmp\"\n" +
		"line=$(grep -E '[ *]" + asset + "$' checksums.txt) || { echo 'checksums.txt has no entry for " + asset + "' >&2; exit 1; }\n" +
		"printf '%s\\n' \"$line\" | sha256sum -c - >/dev/null 2>&1 || { echo 'checksum mismatch' >&2; exit 1; }\n" +
		"chmod 0755 " + asset + "\n" +
		"mv -f " + asset + " " + binPath + ".new\n"
}

func downloadBinary(ctx context.Context, s *Session, src *binarySource, script string) error {
	res, err := s.R.Run(ctx, script, nil)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", src.version, err)
	}
	if res.Code != 0 {
		return fmt.Errorf("downloading %s: %s", src.version, failureText(res))
	}
	return nil
}
