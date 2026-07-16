package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeS3 struct {
	buckets     []types.Bucket
	objects     map[string][]byte
	types       map[string]string
	headErr     error
	putErr      error
	getErr      error
	deleteErr   error
	deletedKeys []string
	getRanges   []string
}

func (f *fakeS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	if f.types == nil {
		f.types = make(map[string]string)
	}
	f.types[aws.ToString(input.Key)] = aws.ToString(input.ContentType)
	payload, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	f.objects[aws.ToString(input.Key)] = payload
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	payload, exists := f.objects[aws.ToString(input.Key)]
	if !exists {
		return nil, &types.NotFound{}
	}
	now := time.Now().UTC()
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(payload))), ContentType: aws.String(f.types[aws.ToString(input.Key)]),
		LastModified: &now,
	}, nil
}

func (f *fakeS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	payload, exists := f.objects[aws.ToString(input.Key)]
	if !exists {
		return nil, &types.NoSuchKey{}
	}
	requestedRange := strings.TrimSpace(aws.ToString(input.Range))
	f.getRanges = append(f.getRanges, requestedRange)
	if requestedRange != "" {
		bounds := strings.SplitN(strings.TrimPrefix(requestedRange, "bytes="), "-", 2)
		if len(bounds) != 2 {
			return nil, errors.New("invalid fake S3 range")
		}
		start, startErr := strconv.ParseInt(bounds[0], 10, 64)
		end, endErr := strconv.ParseInt(bounds[1], 10, 64)
		if startErr != nil || endErr != nil || start < 0 || end < start || start >= int64(len(payload)) {
			return nil, errors.New("invalid fake S3 range")
		}
		if end >= int64(len(payload)) {
			end = int64(len(payload)) - 1
		}
		payload = payload[start : end+1]
	}
	now := time.Now().UTC()
	return &s3.GetObjectOutput{
		Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: aws.Int64(int64(len(payload))),
		ContentType: aws.String(f.types[aws.ToString(input.Key)]), LastModified: &now,
	}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	key := aws.ToString(input.Key)
	delete(f.objects, key)
	delete(f.types, key)
	f.deletedKeys = append(f.deletedKeys, key)
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListBuckets(context.Context, *s3.ListBucketsInput, ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	return &s3.ListBucketsOutput{Buckets: f.buckets}, nil
}

func (f *fakeS3) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &s3.HeadBucketOutput{}, nil
}

func TestS3StoreKeepsObjectsPrivateBehindServiceURL(t *testing.T) {
	client := &fakeS3{objects: make(map[string][]byte), types: make(map[string]string)}
	mediaStore, err := newStore(&s3Backend{client: client, bucket: "maxposty-media"}, "https://maxposty.ru")
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	file, err := mediaStore.Save(context.Background(), "unsafe/ignored.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(file.URL, "https://maxposty.ru/media/") || strings.Contains(file.URL, "s3-") {
		t.Fatalf("public media URL leaks S3 endpoint: %q", file.URL)
	}
	object, err := mediaStore.Open(context.Background(), file.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = object.Body.Close() }()
	stored, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, encoded.Bytes()) || object.MIMEType != "image/png" {
		t.Fatalf("stored object metadata or bytes differ: mime=%q size=%d", object.MIMEType, len(stored))
	}
}

func TestS3RangeReadUsesProviderRange(t *testing.T) {
	client := &fakeS3{
		objects: map[string][]byte{"clip.mp4": []byte("0123456789")},
		types:   map[string]string{"clip.mp4": "video/mp4"},
	}
	mediaStore, err := newStore(&s3Backend{client: client, bucket: "maxposty-media"}, "https://maxposty.ru")
	if err != nil {
		t.Fatal(err)
	}
	object, err := mediaStore.OpenRange(context.Background(), "clip.mp4", 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = object.Body.Close() }()
	payload, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "2345" || object.Size != 4 || object.TotalSize != 10 {
		t.Fatalf("range object = %q size=%d total=%d", payload, object.Size, object.TotalSize)
	}
	if len(client.getRanges) != 1 || client.getRanges[0] != "bytes=2-5" {
		t.Fatalf("S3 requested ranges = %v", client.getRanges)
	}
}

func TestResolveBucketRequiresExplicitNameOnlyForMultipleBuckets(t *testing.T) {
	ctx := context.Background()
	one := &fakeS3{buckets: []types.Bucket{{Name: aws.String("maxposty-media")}}}
	if bucket, err := resolveBucket(ctx, one, ""); err != nil || bucket != "maxposty-media" {
		t.Fatalf("single bucket resolution = %q, %v", bucket, err)
	}
	multiple := &fakeS3{buckets: []types.Bucket{{Name: aws.String("one-bucket")}, {Name: aws.String("two-bucket")}}}
	if _, err := resolveBucket(ctx, multiple, ""); err == nil || !strings.Contains(err.Error(), "multiple buckets") {
		t.Fatalf("multiple buckets error = %v", err)
	}
	if bucket, err := resolveBucket(ctx, multiple, "two-bucket"); err != nil || bucket != "two-bucket" {
		t.Fatalf("explicit bucket resolution = %q, %v", bucket, err)
	}
	if bucket, err := resolveBucket(ctx, multiple, "media.maxposty.ru"); err != nil || bucket != "media.maxposty.ru" {
		t.Fatalf("dotted bucket resolution = %q, %v", bucket, err)
	}
	for _, invalid := range []string{"Uppercase", "under_score", "two..dots", "dash-.dot", "127.0.0.1"} {
		if _, err := resolveBucket(ctx, multiple, invalid); err == nil {
			t.Fatalf("invalid bucket %q was accepted", invalid)
		}
	}
}

func TestVerifyBucketFailsFast(t *testing.T) {
	client := &fakeS3{headErr: errors.New("access denied")}
	err := verifyBucket(context.Background(), client, "maxposty-media")
	if err == nil || !strings.Contains(err.Error(), "maxposty-media") || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("verify bucket error = %v", err)
	}
}

func TestVerifyBucketChecksWriteReadAndDelete(t *testing.T) {
	client := &fakeS3{objects: make(map[string][]byte), types: make(map[string]string)}
	if err := verifyBucket(context.Background(), client, "maxposty-media"); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedKeys) != 1 || len(client.objects) != 0 {
		t.Fatalf("S3 probe cleanup: deleted=%v remaining=%d", client.deletedKeys, len(client.objects))
	}
}

func TestVerifyBucketFailsWhenObjectPermissionsAreIncomplete(t *testing.T) {
	for name, client := range map[string]*fakeS3{
		"put":    {putErr: errors.New("put denied")},
		"get":    {objects: make(map[string][]byte), types: make(map[string]string), getErr: errors.New("get denied")},
		"delete": {objects: make(map[string][]byte), types: make(map[string]string), deleteErr: errors.New("delete denied")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyBucket(context.Background(), client, "maxposty-media"); err == nil {
				t.Fatal("incomplete object permissions were accepted")
			}
		})
	}
}

func TestHostkeyRegionAndEndpointValidation(t *testing.T) {
	if got := regionFromEndpoint("https://s3-nl.hostkey.com"); got != "nl" {
		t.Fatalf("HOSTKEY region = %q, want nl", got)
	}
	if _, err := validateS3Endpoint("http://s3-nl.hostkey.com"); err == nil {
		t.Fatal("insecure non-loopback endpoint was accepted")
	}
	if got, err := validateS3Endpoint("s3-nl.hostkey.com"); err != nil || got != "https://s3-nl.hostkey.com" {
		t.Fatalf("bare HOSTKEY endpoint = %q, %v", got, err)
	}
	if got, err := validateS3Endpoint("http://127.0.0.1:9000/"); err != nil || got != "http://127.0.0.1:9000" {
		t.Fatalf("local test endpoint = %q, %v", got, err)
	}
}

func TestS3RegionValidation(t *testing.T) {
	if s3RegionPattern.MatchString("region with spaces") {
		t.Fatal("region with spaces was accepted")
	}
	if !s3RegionPattern.MatchString("nl") {
		t.Fatal("HOSTKEY region was rejected")
	}
}
