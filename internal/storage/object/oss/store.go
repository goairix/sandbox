package oss

import (
	"context"
	"io"
	"time"

	alioss "github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Endpoint  string // e.g. "oss-cn-hangzhou.aliyuncs.com"
	AccessKey string
	SecretKey string
}

type Store struct {
	bucket *alioss.Bucket
}

func New(opts Options) (*Store, error) {
	client, err := alioss.New(opts.Endpoint, opts.AccessKey, opts.SecretKey)
	if err != nil {
		return nil, err
	}
	bucket, err := client.Bucket(opts.Bucket)
	if err != nil {
		return nil, err
	}
	return &Store{bucket: bucket}, nil
}

func (s *Store) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	return s.bucket.PutObject(key, reader)
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return s.bucket.GetObject(key)
}

func (s *Store) Delete(_ context.Context, key string) error {
	return s.bucket.DeleteObject(key)
}

func (s *Store) List(_ context.Context, prefix string) ([]object.ObjectInfo, error) {
	result, err := s.bucket.ListObjects(alioss.Prefix(prefix))
	if err != nil {
		return nil, err
	}

	var objs []object.ObjectInfo
	for _, obj := range result.Objects {
		objs = append(objs, object.ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}
	return objs, nil
}

func (s *Store) Exists(_ context.Context, key string) (bool, error) {
	return s.bucket.IsObjectExist(key)
}

func (s *Store) PresignedPutURL(_ context.Context, key string, expires time.Duration) (string, error) {
	return s.bucket.SignURL(key, alioss.HTTPPut, int64(expires.Seconds()))
}

func (s *Store) PresignedGetURL(_ context.Context, key string, expires time.Duration) (string, error) {
	return s.bucket.SignURL(key, alioss.HTTPGet, int64(expires.Seconds()))
}
