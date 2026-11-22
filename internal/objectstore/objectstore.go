// Package objectstore is the client for the S3-compatible storage that backup destinations name. It uses path-style
// addressing and signs every request with SigV4.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"berthy/internal/manifest"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
	Meta     map[string]string // user metadata, keys lower-case without the x-amz-meta- prefix
}

// Config is a parsed destination with its credentials.
type Config struct {
	Endpoint  string // "https://host[:port]"; empty => https://s3.amazonaws.com
	Region    string // empty => "us-east-1"
	Bucket    string
	AccessKey string
	SecretKey string
}

type Store interface {
	// Put streams r to key. The size is unknown; large bodies use multipart upload. When r fails, ctx is cancelled or
	// the upload fails, the multipart upload is aborted and no object is left at key. A failed Put never touches an
	// object that was already at key before it started; keys are expected to be unique per upload.
	Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error) // ErrNotFound when absent
	// List is recursive and sorted by key. It uses the listing response only: no per-object Stat. Meta is empty; call Stat for metadata.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Delete(ctx context.Context, key string) error // deleting a missing key is not an error
}

var ErrNotFound = errors.New("object not found")

// The messages a failed request reports. Errors carry only these, never the client's own error: minio's errors hold
// request URLs whose query strings carry signatures.
var (
	errUnavailable   = errors.New("object storage unavailable")
	errAccessDenied  = errors.New("access denied")
	errBucketMissing = errors.New("bucket not found")
	errRequestFailed = errors.New("object storage request failed")
	errSource        = errors.New("reading the source failed")
)

const (
	defaultEndpoint = "https://s3.amazonaws.com"
	defaultRegion   = "us-east-1"
	// cleanupTimeout bounds the abort and removal after a failed upload, which run even when the caller's context is
	// cancelled.
	cleanupTimeout = 30 * time.Second
	maxMetaValue   = 1024
	maxMetaBytes   = 2048
	// partSize is the size of one part of an upload of unknown length, and the memory the client holds for it. minio
	// would pick about 537 MiB for an unknown length. At 64 MiB, an object can be up to about 625 GiB.
	partSize = 64 << 20
)

var metaKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type s3Store struct {
	client   *minio.Client
	bucket   string
	region   string
	secure   bool
	partSize int64
}

// New returns an S3 store using path-style addressing. Errors never contain the secret key.
func New(c Config) (Store, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if err := manifest.ValidEndpoint(endpoint); err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}
	secure, host, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if c.Bucket == "" {
		return nil, errors.New("bucket is required")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("access key and secret key are required")
	}
	region := c.Region
	if region == "" {
		region = defaultRegion
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, errors.New("invalid object storage configuration")
	}
	return &s3Store{client: client, bucket: c.Bucket, region: region, secure: secure, partSize: partSize}, nil
}

// parseEndpoint splits an endpoint that manifest.ValidEndpoint has accepted into its TLS flag and host:port.
func parseEndpoint(raw string) (secure bool, host string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, "", errors.New("invalid endpoint")
	}
	return u.Scheme == "https", u.Host, nil
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error) {
	userMeta, err := normalizeMeta(meta)
	if err != nil {
		return 0, err
	}
	existed, err := s.exists(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("upload failed: %w", err)
	}
	src := &countingReader{r: r}
	_, err = s.client.PutObject(ctx, s.bucket, key, src, -1, minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		UserMetadata: userMeta,
		PartSize:     uint64(s.partSize),
		// Over plain http, minio signs the payload in SigV4 streaming chunks. Plain http is allowed only for loopback
		// endpoints, where per-chunk signing adds nothing, and the fake S3 server that the tests use does not decode
		// chunked bodies. Requests stay signed; only the payload is sent UNSIGNED-PAYLOAD.
		DisableContentSha256: !s.secure,
	})
	if err != nil {
		s.undoFailedPut(ctx, key, existed)
		return 0, uploadFailure(ctx, src, err)
	}
	return src.n, nil
}

// undoFailedPut aborts the multipart upload a failed Put left behind and, when the key did not exist before the Put,
// removes an object that a lost final response may have completed. It uses a context the caller cannot cancel, because
// the caller's context is often the reason the Put failed.
func (s *s3Store) undoFailedPut(ctx context.Context, key string, existed bool) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = s.client.RemoveIncompleteUpload(cleanup, s.bucket, key)
	if !existed {
		_ = s.client.RemoveObject(cleanup, s.bucket, key, minio.RemoveObjectOptions{})
	}
}

func (s *s3Store) exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	}
	return false, err
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, s.failure(ctx, err)
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, ObjectInfo{}, s.failure(ctx, err)
	}
	return &objectReader{ctx: ctx, r: obj}, toInfo(key, st), nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	st, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, s.failure(ctx, err)
	}
	return toInfo(key, st), nil
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var out []ObjectInfo
	for o := range s.client.ListObjects(listCtx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, s.failure(ctx, o.Err)
		}
		out = append(out, ObjectInfo{Key: o.Key, Size: o.Size, Modified: o.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err == nil || isNotFound(err) {
		return nil
	}
	return s.failure(ctx, err)
}

// failure maps a client error to a short error. A missing object is ErrNotFound.
func (s *s3Store) failure(ctx context.Context, err error) error {
	if isNotFound(err) {
		return ErrNotFound
	}
	return classify(ctx, err)
}

// classify maps a client error to one of the short messages. A cancelled context is reported as the context's error.
func classify(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "NoSuchBucket":
			return errBucketMissing
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccountProblem":
			return errAccessDenied
		case "InternalError", "ServiceUnavailable", "SlowDown", "RequestTimeout":
			return errUnavailable
		}
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			return errAccessDenied
		case resp.StatusCode >= 500:
			return errUnavailable
		}
		return errRequestFailed
	}
	var transport *url.Error
	if errors.As(err, &transport) {
		return errUnavailable
	}
	return errRequestFailed
}

func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	return resp.Code == "NoSuchKey" || resp.StatusCode == 404 && resp.Code != "NoSuchBucket"
}

func uploadFailure(ctx context.Context, src *countingReader, err error) error {
	if src.err != nil && ctx.Err() == nil {
		return fmt.Errorf("upload failed: %w", errSource)
	}
	return fmt.Errorf("upload failed: %w", classify(ctx, err))
}

// normalizeMeta checks user metadata before anything is sent and lower-cases its keys. Values are printable ASCII, as
// HTTP headers require.
func normalizeMeta(meta map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(meta))
	total := 0
	for k, v := range meta {
		lk := strings.ToLower(k)
		total += len(lk) + len(v)
		if !metaKey.MatchString(lk) || len(v) > maxMetaValue || strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
			return nil, errors.New("invalid object metadata")
		}
		if _, dup := out[lk]; dup {
			return nil, errors.New("invalid object metadata")
		}
		out[lk] = v
	}
	if total > maxMetaBytes {
		return nil, errors.New("invalid object metadata")
	}
	return out, nil
}

// metaName returns the lower-case name of a user metadata key as minio reports it.
func metaName(k string) string {
	return strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")
}

func toInfo(key string, st minio.ObjectInfo) ObjectInfo {
	meta := make(map[string]string, len(st.UserMetadata))
	for k, v := range st.UserMetadata {
		meta[metaName(k)] = v
	}
	return ObjectInfo{Key: key, Size: st.Size, Modified: st.LastModified, Meta: meta}
}

// countingReader counts the bytes a Put reads and keeps the first error its source returned.
type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

// objectReader reports read failures as short errors, so a request URL never reaches the caller.
type objectReader struct {
	ctx context.Context
	r   io.ReadCloser
}

func (o *objectReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	if err != nil && err != io.EOF {
		return n, classify(o.ctx, err)
	}
	return n, err
}

func (o *objectReader) Close() error { return o.r.Close() }
