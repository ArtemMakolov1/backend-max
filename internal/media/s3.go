package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var (
	hostkeyRegionPattern = regexp.MustCompile(`^s3-([a-z0-9-]+)\.hostkey\.com$`)
	s3BucketPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3RegionPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// S3Config configures private S3-compatible object storage. Objects are never
// exposed directly: clients keep using the authenticated /media/{key} route.
type S3Config struct {
	Endpoint   string
	Region     string
	KeyID      string
	SecretKey  string
	Bucket     string
	HTTPClient aws.HTTPClient
}

type s3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	ListBuckets(context.Context, *s3.ListBucketsInput, ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
}

type s3Backend struct {
	client s3API
	bucket string
}

// NewS3 creates a private S3-backed media store. If Bucket is empty, startup
// succeeds only when the credentials expose exactly one bucket.
func NewS3(ctx context.Context, cfg S3Config, publicBaseURL string) (*Store, error) {
	endpoint, err := validateS3Endpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	accessKey := strings.TrimSpace(cfg.KeyID)
	secretKey := strings.TrimSpace(cfg.SecretKey)
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("S3 access key and secret key are required")
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = regionFromEndpoint(endpoint)
	}
	if !s3RegionPattern.MatchString(region) {
		return nil, errors.New("S3_REGION contains invalid characters")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	awsConfig := aws.Config{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		HTTPClient:  httpClient,
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
		options.RetryMaxAttempts = 3
		options.RetryMode = aws.RetryModeStandard
	})
	bucket, err := resolveBucket(ctx, client, cfg.Bucket)
	if err != nil {
		return nil, err
	}
	if err := verifyBucket(ctx, client, bucket); err != nil {
		return nil, err
	}
	return newStore(&s3Backend{client: client, bucket: bucket}, publicBaseURL)
}

func verifyBucket(ctx context.Context, client s3API, bucket string) error {
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return fmt.Errorf("access S3 bucket %q: %w", bucket, err)
	}

	probeID := make([]byte, 16)
	if _, err := rand.Read(probeID); err != nil {
		return fmt.Errorf("create S3 access probe: %w", err)
	}
	key := "maxposty-access-probe-" + hex.EncodeToString(probeID)
	payload := []byte("maxposty-private-s3-access-probe")
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload),
		ContentLength: aws.Int64(int64(len(payload))), ContentType: aws.String("application/octet-stream"),
		CacheControl: aws.String("private, no-store"),
	}); err != nil {
		return fmt.Errorf("write S3 access probe: %w", err)
	}

	cleanup := func() error {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
			return fmt.Errorf("delete S3 access probe: %w", err)
		}
		return nil
	}
	result, getErr := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if getErr != nil {
		return errors.Join(fmt.Errorf("read S3 access probe: %w", getErr), cleanup())
	}
	actual, readErr := io.ReadAll(io.LimitReader(result.Body, int64(len(payload)+1)))
	closeErr := result.Body.Close()
	cleanupErr := cleanup()
	if readErr != nil || closeErr != nil || cleanupErr != nil {
		return errors.Join(
			wrapS3ProbeError("read S3 access probe body", readErr),
			wrapS3ProbeError("close S3 access probe body", closeErr),
			cleanupErr,
		)
	}
	if !bytes.Equal(actual, payload) {
		return errors.New("S3 access probe returned different content")
	}
	return nil
}

func wrapS3ProbeError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func validateS3Endpoint(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	// HOSTKEY shows the endpoint both as an URL and as a bare hostname in its
	// clients. A bare hostname always means HTTPS; plaintext remains forbidden.
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("S3_HOST must be an absolute endpoint without credentials, query or fragment")
	}
	if parsed.Path != "" {
		return "", errors.New("S3_HOST must not contain a path")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopbackS3Host(parsed.Hostname())) {
		return "", errors.New("S3_HOST must use HTTPS outside localhost")
	}
	return parsed.String(), nil
}

func isLoopbackS3Host(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func regionFromEndpoint(endpoint string) string {
	parsed, _ := url.Parse(endpoint)
	if matches := hostkeyRegionPattern.FindStringSubmatch(strings.ToLower(parsed.Hostname())); len(matches) == 2 {
		return matches[1]
	}
	return "us-east-1"
}

func resolveBucket(ctx context.Context, client s3API, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if !validS3Bucket(configured) {
			return "", errors.New("S3_BUCKET contains invalid characters")
		}
		return configured, nil
	}
	result, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return "", fmt.Errorf("list S3 buckets: %w", err)
	}
	available := make([]string, 0, len(result.Buckets))
	for _, bucket := range result.Buckets {
		name := strings.TrimSpace(aws.ToString(bucket.Name))
		if validS3Bucket(name) {
			available = append(available, name)
		}
	}
	if len(available) == 0 {
		return "", errors.New("S3_BUCKET is empty and no bucket is available for these credentials")
	}
	if len(available) != 1 {
		return "", errors.New("S3_BUCKET is required when credentials can access multiple buckets")
	}
	return available[0], nil
}

func validS3Bucket(bucket string) bool {
	return s3BucketPattern.MatchString(bucket) &&
		!strings.Contains(bucket, "..") &&
		!strings.Contains(bucket, ".-") &&
		!strings.Contains(bucket, "-.") &&
		net.ParseIP(bucket) == nil
}

func (b *s3Backend) Put(ctx context.Context, key, mimeType string, size int64, reader io.Reader) error {
	if !validFilename(key) {
		return errors.New("invalid media filename")
	}
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          reader,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(mimeType),
		CacheControl:  aws.String("private, no-store"),
	})
	if err != nil {
		return fmt.Errorf("put S3 object: %w", err)
	}
	return nil
}

func (b *s3Backend) Head(ctx context.Context, key string) (objectInfo, error) {
	if !validFilename(key) {
		return objectInfo{}, errors.New("invalid media filename")
	}
	result, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		if isS3NotFound(err) {
			return objectInfo{}, os.ErrNotExist
		}
		return objectInfo{}, fmt.Errorf("head S3 object: %w", err)
	}
	return objectInfo{
		MIMEType: aws.ToString(result.ContentType), Size: aws.ToInt64(result.ContentLength),
		LastModified: aws.ToTime(result.LastModified),
	}, nil
}

func (b *s3Backend) Open(ctx context.Context, key string) (Object, error) {
	if !validFilename(key) {
		return Object{}, errors.New("invalid media filename")
	}
	result, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		if isS3NotFound(err) {
			return Object{}, os.ErrNotExist
		}
		return Object{}, fmt.Errorf("get S3 object: %w", err)
	}
	return Object{
		Body: result.Body, Filename: key, MIMEType: aws.ToString(result.ContentType),
		Size: aws.ToInt64(result.ContentLength), LastModified: aws.ToTime(result.LastModified),
	}, nil
}

func isS3NotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch strings.ToLower(apiError.ErrorCode()) {
		case "nosuchkey", "notfound", "404":
			return true
		}
	}
	return false
}
