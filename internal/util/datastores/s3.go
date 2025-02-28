package datastores

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/beeper/babbleserv/internal/config"
)

var _ Datastore = (*S3Store)(nil)

type S3Store struct {
	baseDatastore

	client *minio.Client

	bucket,
	endpoint,
	accessKeyID,
	secretAccessKey string
}

func NewS3Store() *S3Store {
	return &S3Store{}
}

func getString(data map[string]any, key string) string {
	if v, found := data[key]; found {
		if ret, ok := v.(string); ok {
			return ret
		}
	}
	return ""
}

func (s *S3Store) Configure(config config.BabbleConfig, data map[string]any) {
	s.baseDatastore.Configure(data)

	s.bucket = getString(data, "bucket")
	s.endpoint = getString(data, "endpoint")
	s.accessKeyID = getString(data, "accessKeyID")
	s.secretAccessKey = getString(data, "secretAccessKey")

	client, err := minio.New(s.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s.accessKeyID, s.secretAccessKey, ""),
		Secure: true,
	})
	if err != nil {
		panic(err)
	}

	exists, err := client.BucketExists(context.Background(), s.bucket)
	if err != nil {
		// panic(err)
	} else if !exists {
		if config.SecretSwitches.AutoCreateDatastoreBuckets {
			err = client.MakeBucket(context.Background(), s.bucket, minio.MakeBucketOptions{})
			if err != nil {
				panic(err)
			}
		} else {
			panic(fmt.Errorf("bucket does not exist: %s", s.bucket))
		}
	}

	s.client = client
}

func (s *S3Store) GetObjectPresignedURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	if u, err := s.client.PresignedGetObject(ctx, s.bucket, key, expires, nil); err != nil {
		return "", err
	} else {
		return u.String(), nil
	}
}

func (s *S3Store) PutObjectPresignedURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	if u, err := s.client.PresignedPutObject(ctx, s.bucket, key, expires); err != nil {
		return "", err
	} else {
		return u.String(), nil
	}
}

func (s *S3Store) GetObject(ctx context.Context, key string) (io.Reader, error) {
	return s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
}

func (s *S3Store) GetObjectInfo(ctx context.Context, key string) (ObjectInfo, error) {
	var info ObjectInfo
	if objInfo, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		return info, err
	} else {
		info.Size = objInfo.Size
		info.ContentType = objInfo.ContentType
	}
	return info, nil
}

func (s *S3Store) PutObject(ctx context.Context, key string, src io.Reader, info ObjectInfo) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, src, info.Size, minio.PutObjectOptions{
		ContentType: info.ContentType,
	})
	return err
}
