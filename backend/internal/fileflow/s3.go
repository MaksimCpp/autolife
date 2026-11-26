package fileflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Config struct {
	Endpoint       string
	PublicEndpoint string
	Bucket         string
	Region         string
	AccessKey      string
	SecretKey      string
	AllowHTTP      bool
}

type S3 struct {
	client *s3.Client
	signer *s3.PresignClient
	bucket string
}

func NewS3(config S3Config) (*S3, error) {
	if config.Bucket == "" || config.Region == "" || config.AccessKey == "" || config.SecretKey == "" {
		return nil, errors.New("S3 bucket, region and credentials are required")
	}
	if config.PublicEndpoint == "" {
		config.PublicEndpoint = config.Endpoint
	}
	for _, address := range []string{config.Endpoint, config.PublicEndpoint} {
		u, err := url.Parse(address)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
			(u.Path != "" && u.Path != "/") ||
			(u.Scheme != "https" && !(config.AllowHTTP && u.Scheme == "http")) {
			return nil, errors.New("invalid S3 endpoint")
		}
	}
	cfg := aws.Config{
		Region:      config.Region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, "")),
		// Never follow a redirect to an origin outside the configured storage service.
		HTTPClient: &http.Client{
			Timeout: 2 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}
	newClient := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(cfg, func(options *s3.Options) {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		})
	}
	return &S3{
		client: newClient(config.Endpoint),
		signer: s3.NewPresignClient(newClient(config.PublicEndpoint)),
		bucket: config.Bucket,
	}, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string, sum [sha256.Size]byte) error {
	if size < 1 || size > MaxFileBytes {
		return fmt.Errorf("object size must be in 1..%d", MaxFileBytes)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size),
		ContentType: aws.String(contentType), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	return err
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *S3) PresignDownload(ctx context.Context, key, name string, expiresAt time.Time) (string, error) {
	ttl := min(time.Until(expiresAt), 5*time.Minute).Truncate(time.Second)
	if ttl < time.Second {
		return "", ErrNotFound
	}
	// The reference expiry is checked against the actual signing timestamp,
	// since credential resolution can take time.
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
		ResponseContentDisposition: aws.String(mime.FormatMediaType("attachment", map[string]string{"filename": name})),
		ResponseContentType:        aws.String("application/octet-stream"),
	}
	req, err := s.signer.PresignGetObject(ctx, input, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	signed, err := url.Parse(req.URL)
	if err != nil {
		return "", errors.New("invalid S3 download URL")
	}
	stamp, err := time.Parse("20060102T150405Z", signed.Query().Get("X-Amz-Date"))
	if err != nil || stamp.Add(ttl).After(expiresAt) {
		return "", ErrNotFound
	}
	return req.URL, nil
}

// orphanPage examines only server-generated file keys. A grace period protects
// in-flight requests and eventually recovers a PUT that completed after a timeout.
func (s *S3) orphanPage(ctx context.Context, cursor *string, cutoff time.Time, visit func(string, string) error) (*string, error) {
	page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String("files/"),
		MaxKeys: aws.Int32(100), ContinuationToken: cursor,
	})
	if err != nil {
		return nil, err
	}
	for _, object := range page.Contents {
		key := aws.ToString(object.Key)
		id, ok := strings.CutPrefix(key, "files/")
		if !ok || !validID(id) || object.LastModified == nil || !object.LastModified.Before(cutoff) {
			continue
		}
		if err := visit(id, key); err != nil {
			return cursor, err
		}
	}
	if aws.ToBool(page.IsTruncated) && page.NextContinuationToken == nil {
		return cursor, ErrUnavailable
	}
	return page.NextContinuationToken, nil
}
