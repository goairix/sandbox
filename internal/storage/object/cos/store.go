package cos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Region    string
	SecretID  string
	SecretKey string
}

type Store struct {
	client    *cos.Client
	secretID  string
	secretKey string
}

func New(opts Options) (*Store, error) {
	bucketURL, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", opts.Bucket, opts.Region))
	if err != nil {
		return nil, err
	}
	serviceURL, err := url.Parse(fmt.Sprintf("https://cos.%s.myqcloud.com", opts.Region))
	if err != nil {
		return nil, err
	}

	client := cos.NewClient(&cos.BaseURL{
		BucketURL:  bucketURL,
		ServiceURL: serviceURL,
	}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  opts.SecretID,
			SecretKey: opts.SecretKey,
		},
	})

	return &Store{
		client:    client,
		secretID:  opts.SecretID,
		secretKey: opts.SecretKey,
	}, nil
}

func (s *Store) Put(ctx context.Context, key string, reader io.Reader, _ int64) error {
	_, err := s.client.Object.Put(ctx, key, reader, nil)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.client.Object.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.Object.Delete(ctx, key)
	return err
}

func (s *Store) List(ctx context.Context, prefix string) ([]object.ObjectInfo, error) {
	opt := &cos.BucketGetOptions{
		Prefix: prefix,
	}
	result, _, err := s.client.Bucket.Get(ctx, opt)
	if err != nil {
		return nil, err
	}

	var objs []object.ObjectInfo
	for _, item := range result.Contents {
		modTime, _ := time.Parse(time.RFC3339, item.LastModified)
		objs = append(objs, object.ObjectInfo{
			Key:          item.Key,
			Size:         int64(item.Size),
			LastModified: modTime,
		})
	}
	return objs, nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	ok, err := s.client.Object.IsExist(ctx, key)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (s *Store) PresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodPut, key, s.secretID, s.secretKey, expires, nil)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}

func (s *Store) PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodGet, key, s.secretID, s.secretKey, expires, nil)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}
