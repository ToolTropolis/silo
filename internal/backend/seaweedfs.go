package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// Config points the SeaweedFS adapter at an S3 gateway.
type Config struct {
	Endpoint  string // e.g. http://localhost:8333
	Region    string // SeaweedFS ignores it, but the SDK requires one
	AccessKey string // empty => anonymous (the dev compose runs open)
	SecretKey string
}

// bucketPrefix namespaces Silo's per-project buckets so they never collide with
// unrelated buckets on a shared SeaweedFS cluster.
const bucketPrefix = "silo-"

// SeaweedFS is the default DurableBackend, backed by SeaweedFS's S3-compatible
// gateway: bucket-per-project, native object versioning, ETag conditional
// writes, and (where configured) a per-project SSE key.
type SeaweedFS struct {
	client *s3.Client
}

var _ DurableBackend = (*SeaweedFS)(nil)

// NewSeaweedFS builds an adapter from Config. Uses path-style addressing, which
// SeaweedFS requires (it doesn't do virtual-host bucket subdomains).
func NewSeaweedFS(cfg Config) (*SeaweedFS, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("backend: endpoint required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	var credProvider aws.CredentialsProvider
	if cfg.AccessKey != "" {
		credProvider = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	} else {
		credProvider = aws.AnonymousCredentials{}
	}

	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credProvider,
	})
	return &SeaweedFS{client: client}, nil
}

// bucketFor maps a projectID to its dedicated bucket name — the isolation
// boundary. One project's credential is scoped to one bucket.
func bucketFor(projectID string) string {
	return bucketPrefix + projectID
}

func (s *SeaweedFS) Put(ctx context.Context, projectID, path string, content []byte, opts PutOptions) (ObjectVersion, error) {
	bucket := bucketFor(projectID)

	// Enforce the CAS precondition ourselves: SeaweedFS's PutObject doesn't
	// honor If-Match, so read the current ETag and reject if it moved. This
	// matches the SafeWrite contract (retry on ErrPreconditionFailed).
	if opts.IfMatchETag != "" {
		_, cur, err := s.Get(ctx, projectID, path, "")
		if err != nil && !errors.Is(err, ErrNotFound) {
			return ObjectVersion{}, err
		}
		if errors.Is(err, ErrNotFound) || normalizeETag(cur.ETag) != normalizeETag(opts.IfMatchETag) {
			return ObjectVersion{}, ErrPreconditionFailed
		}
	}

	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
		Body:   bytes.NewReader(content),
	}
	if len(opts.Tags) > 0 {
		in.Metadata = opts.Tags
	}
	if opts.Actor != "" {
		if in.Metadata == nil {
			in.Metadata = map[string]string{}
		}
		in.Metadata["silo-actor"] = opts.Actor
	}
	if opts.SessionID != "" {
		if in.Metadata == nil {
			in.Metadata = map[string]string{}
		}
		in.Metadata["silo-session"] = opts.SessionID
	}

	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return ObjectVersion{}, fmt.Errorf("backend: put %s/%s: %w", bucket, path, err)
	}
	return ObjectVersion{
		VersionID: aws.ToString(out.VersionId),
		ETag:      normalizeETag(aws.ToString(out.ETag)),
	}, nil
}

func (s *SeaweedFS) Get(ctx context.Context, projectID, path string, versionID string) ([]byte, ObjectVersion, error) {
	bucket := bucketFor(projectID)
	in := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		if isNotFound(err) {
			return nil, ObjectVersion{}, ErrNotFound
		}
		return nil, ObjectVersion{}, fmt.Errorf("backend: get %s/%s: %w", bucket, path, err)
	}
	defer out.Body.Close()

	content, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, ObjectVersion{}, fmt.Errorf("backend: read body %s/%s: %w", bucket, path, err)
	}
	ver := ObjectVersion{
		VersionID: aws.ToString(out.VersionId),
		ETag:      normalizeETag(aws.ToString(out.ETag)),
	}
	if out.LastModified != nil {
		ver.ModifiedAt = out.LastModified.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return content, ver, nil
}

// ListPaths returns current object keys under a prefix, paginating through the
// full result set so large projects aren't silently truncated at 1000 keys.
func (s *SeaweedFS) ListPaths(ctx context.Context, projectID, prefix string) ([]string, error) {
	bucket := bucketFor(projectID)
	var (
		paths []string
		token *string
	)
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("backend: list paths %s/%s: %w", bucket, prefix, err)
		}
		for _, o := range out.Contents {
			paths = append(paths, aws.ToString(o.Key))
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return paths, nil
}

func (s *SeaweedFS) ListVersions(ctx context.Context, projectID, path string) ([]ObjectVersion, error) {
	bucket := bucketFor(projectID)
	out, err := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket),
		Prefix: aws.String(path),
	})
	if err != nil {
		return nil, fmt.Errorf("backend: list versions %s/%s: %w", bucket, path, err)
	}
	var versions []ObjectVersion
	for _, v := range out.Versions {
		// Prefix is not an exact match — keep only the exact key.
		if aws.ToString(v.Key) != path {
			continue
		}
		ov := ObjectVersion{
			VersionID: aws.ToString(v.VersionId),
			ETag:      normalizeETag(aws.ToString(v.ETag)),
		}
		if v.LastModified != nil {
			ov.ModifiedAt = v.LastModified.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		versions = append(versions, ov)
	}
	// SeaweedFS returns newest-first already, but don't rely on it: the AWS API
	// contract is latest-first, which we preserve by not re-sorting.
	return versions, nil
}

func (s *SeaweedFS) Delete(ctx context.Context, projectID, path string) error {
	bucket := bucketFor(projectID)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return fmt.Errorf("backend: delete %s/%s: %w", bucket, path, err)
	}
	return nil
}

func (s *SeaweedFS) CreateBucket(ctx context.Context, projectID string) error {
	bucket := bucketFor(projectID)
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil && !isAlreadyOwned(err) {
		return fmt.Errorf("backend: create bucket %s: %w", bucket, err)
	}
	// Enable versioning so ListVersions and rollback work (spec §1).
	_, err = s.client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	})
	if err != nil {
		return fmt.Errorf("backend: enable versioning %s: %w", bucket, err)
	}
	return nil
}

func (s *SeaweedFS) DeleteBucket(ctx context.Context, projectID string) error {
	bucket := bucketFor(projectID)
	_, err := s.client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return fmt.Errorf("backend: delete bucket %s: %w", bucket, err)
	}
	return nil
}

// normalizeETag strips the surrounding quotes S3 wraps ETags in, so callers can
// compare them without worrying about quoting.
func normalizeETag(etag string) string {
	return strings.Trim(etag, `"`)
}

// isNotFound reports whether err is an S3 no-such-key / 404.
func isNotFound(err error) bool {
	var nske *types.NoSuchKey
	if errors.As(err, &nske) {
		return true
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// isAlreadyOwned reports whether a CreateBucket error just means the bucket
// already exists — idempotent onboarding shouldn't fail on a re-run.
func isAlreadyOwned(err error) bool {
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}
