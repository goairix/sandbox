package obs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	huaweiobs "github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Endpoint  string // e.g. "obs.cn-north-4.myhuaweicloud.com"
	AccessKey string
	SecretKey string
}

type Store struct {
	client *huaweiobs.ObsClient
	bucket string
}

func New(opts Options) (*Store, error) {
	client, err := huaweiobs.New(opts.AccessKey, opts.SecretKey, opts.Endpoint)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, bucket: opts.Bucket}, nil
}

func (s *Store) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	input := &huaweiobs.PutObjectInput{
		PutObjectBasicInput: huaweiobs.PutObjectBasicInput{
			ObjectOperationInput: huaweiobs.ObjectOperationInput{
				Bucket: s.bucket,
				Key:    key,
			},
		},
		Body: reader,
	}
	_, err := s.client.PutObject(input)
	return err
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	input := &huaweiobs.GetObjectInput{
		GetObjectMetadataInput: huaweiobs.GetObjectMetadataInput{
			Bucket: s.bucket,
			Key:    key,
		},
	}
	output, err := s.client.GetObject(input)
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	input := &huaweiobs.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    key,
	}
	_, err := s.client.DeleteObject(input)
	return err
}

func (s *Store) List(_ context.Context, prefix string) ([]object.ObjectInfo, error) {
	var result []object.ObjectInfo
	marker := ""

	for {
		input := &huaweiobs.ListObjectsInput{
			Bucket: s.bucket,
			ListObjsInput: huaweiobs.ListObjsInput{
				Prefix: prefix,
			},
			Marker: marker,
		}
		output, err := s.client.ListObjects(input)
		if err != nil {
			return nil, err
		}

		for _, obj := range output.Contents {
			result = append(result, object.ObjectInfo{
				Key:          obj.Key,
				Size:         obj.Size,
				LastModified: obj.LastModified,
			})
		}

		if !output.IsTruncated {
			break
		}
		marker = output.NextMarker
	}

	return result, nil
}

func (s *Store) Exists(_ context.Context, key string) (bool, error) {
	input := &huaweiobs.GetObjectMetadataInput{
		Bucket: s.bucket,
		Key:    key,
	}
	_, err := s.client.GetObjectMetadata(input)
	if err != nil {
		// OBS returns HTTP 404 for non-existent objects
		if obsErr, ok := err.(huaweiobs.ObsError); ok && obsErr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		// Also check for common "NoSuchKey" error code in the message
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) PresignedPutURL(_ context.Context, key string, expires time.Duration) (string, error) {
	input := &huaweiobs.CreateSignedUrlInput{
		Method:  huaweiobs.HttpMethodPut,
		Bucket:  s.bucket,
		Key:     key,
		Expires: int(expires.Seconds()),
	}
	output, err := s.client.CreateSignedUrl(input)
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}

func (s *Store) PresignedGetURL(_ context.Context, key string, expires time.Duration) (string, error) {
	input := &huaweiobs.CreateSignedUrlInput{
		Method:  huaweiobs.HttpMethodGet,
		Bucket:  s.bucket,
		Key:     key,
		Expires: int(expires.Seconds()),
	}
	output, err := s.client.CreateSignedUrl(input)
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}
