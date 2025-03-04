package s3_client

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ipfs/boxo/blockservice/internal"
)

const (
	PART_SIZE   = 100 * 1000 * 1000
	CONCURRENCY = 5
)

type S3Client struct {
	s3BucketName string

	client *s3.Client

	logger *slog.Logger
}

type S3ClientOption func(*S3Client)

func NewS3Client(
	key, secret, region, bucketName, endPoint string,
	opts ...S3ClientOption,
) (*S3Client, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(key, secret, "")),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, s3.WithEndpointResolver(s3.EndpointResolverFromURL(endPoint)))
	helper := &S3Client{
		s3BucketName: bucketName,

		client: client,

		logger: slog.Default(),
	}

	for _, opt := range opts {
		opt(helper)
	}

	if _, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{}); err != nil {
		return nil, err
	}

	return helper, nil
}

func (bs *S3Client) Upload(ctx context.Context, key string, reader io.Reader) error {
	start := time.Now()
	defer func() {
		bs.logger.Debug(
			"s3 upload",
			slog.Any("runtime", time.Since(start).Seconds()),
			slog.Any("key", key),
		)
	}()

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	newUploader := manager.NewUploader(bs.client, func(u *manager.Uploader) {
		u.PartSize = PART_SIZE
		u.Concurrency = CONCURRENCY
	})
	if _, err := newUploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bs.s3BucketName),
		Key:    aws.String(key),
		Body:   reader,
	}); err != nil {
		return err
	}

	return nil
}

func (bs *S3Client) Download(ctx context.Context, key string) (io.Reader, error) {
	start := time.Now()
	defer func() {
		bs.logger.Debug(
			"s3 download",
			slog.Any("runtime", time.Since(start).Seconds()),
			slog.Any("key", key),
		)
	}()

	resp, err := bs.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bs.s3BucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

func (bs *S3Client) Delete(ctx context.Context, key string) error {
	start := time.Now()
	defer func() {
		bs.logger.Debug(
			"s3 delete",
			slog.Any("runtime", time.Since(start).Seconds()),
			slog.Any("key", key),
		)
	}()

	_, err := bs.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bs.s3BucketName),
		Key:    aws.String(key),
	})

	return err
}

func WithLogger(logger *slog.Logger) S3ClientOption {
	return func(bs *S3Client) {
		if logger != nil {
			bs.logger = logger
		}
	}
}

var _ internal.BackUpStorageI = &S3Client{}
