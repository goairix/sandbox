package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	obs "github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
	miniogo "github.com/minio/minio-go/v7"
	miniocredentials "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/goairix/sandbox/internal/config"
)

const (
	RootMarkerProfileMinIO                = "minio-sigv4-path-style-v1"
	RootMarkerProfileHuaweiOBSPublic      = "huawei-obs-public-v1"
	RootMarkerProfileHuaweiOBSPrivate2023 = "huawei-obs-private-2023-v1"
)

// RootMarkerStyle identifies how a provider represents an empty directory.
type RootMarkerStyle string

const (
	// RootMarkerTrailingSlash is a zero-byte object whose key is the exact
	// canonical workspace prefix, including its trailing slash.
	RootMarkerTrailingSlash RootMarkerStyle = "trailing-slash-zero-byte"
)

// RootMarkerOptions are the fixed headers applied to a root marker object.
type RootMarkerOptions struct {
	ContentType string
	Metadata    map[string]string
}

// RootMarkerProfile describes marker behavior only. A profile in this
// registry does not by itself constitute evidence that the corresponding
// mounter profile is approved for production.
type RootMarkerProfile struct {
	ID      string
	Style   RootMarkerStyle
	options RootMarkerOptions
}

var rootMarkerProfiles = map[string]RootMarkerProfile{
	RootMarkerProfileMinIO: {
		ID:    RootMarkerProfileMinIO,
		Style: RootMarkerTrailingSlash,
		options: RootMarkerOptions{
			ContentType: "application/x-directory",
			Metadata:    map[string]string{},
		},
	},
	RootMarkerProfileHuaweiOBSPublic: {
		ID:    RootMarkerProfileHuaweiOBSPublic,
		Style: RootMarkerTrailingSlash,
		options: RootMarkerOptions{
			ContentType: "application/x-directory",
			Metadata:    map[string]string{},
		},
	},
	RootMarkerProfileHuaweiOBSPrivate2023: {
		ID:    RootMarkerProfileHuaweiOBSPrivate2023,
		Style: RootMarkerTrailingSlash,
		options: RootMarkerOptions{
			ContentType: "application/x-directory",
			Metadata:    map[string]string{},
		},
	},
}

// RootMarkerProfileByID returns a copy whose metadata can be safely modified
// by the caller.
func RootMarkerProfileByID(id string) (RootMarkerProfile, error) {
	profile, ok := rootMarkerProfiles[id]
	if !ok {
		return RootMarkerProfile{}, fmt.Errorf("storage: unsupported root marker profile %q", id)
	}
	profile.options = cloneRootMarkerOptions(profile.options)
	return profile, nil
}

// MarkerOptions returns a deep copy of the profile's immutable marker
// options. Modifying the result cannot alter this profile or the registry.
func (p RootMarkerProfile) MarkerOptions() RootMarkerOptions {
	options := cloneRootMarkerOptions(p.options)
	if options.ContentType == "" {
		options.ContentType = "application/x-directory"
	}
	if options.Metadata == nil {
		options.Metadata = map[string]string{}
	}
	return options
}

// WorkspaceObjectClient performs the narrow object operations needed before
// a FUSE workspace is mounted.
type WorkspaceObjectClient interface {
	PutEmptyObject(ctx context.Context, key string, options RootMarkerOptions) error
	HeadObject(ctx context.Context, key string) (bool, error)
	DeleteObject(ctx context.Context, key string) error
}

// PrepareWorkspacePrefix writes and verifies the exact root marker for a
// canonical workspace prefix.
func PrepareWorkspacePrefix(ctx context.Context, client WorkspaceObjectClient, prefix string, profile RootMarkerProfile) error {
	if err := validateCanonicalObjectPrefix(prefix); err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("storage: workspace object client is required")
	}
	if profile.Style != RootMarkerTrailingSlash {
		return fmt.Errorf("storage: unsupported root marker style %q", profile.Style)
	}

	options := profile.MarkerOptions()
	if err := client.PutEmptyObject(ctx, prefix, options); err != nil {
		return fmt.Errorf("storage: put workspace root marker: %w", err)
	}
	exists, err := client.HeadObject(ctx, prefix)
	if err != nil {
		return fmt.Errorf("storage: verify workspace root marker: %w", err)
	}
	if !exists {
		return fmt.Errorf("storage: workspace root marker verification reported missing object")
	}
	return nil
}

func validateCanonicalObjectPrefix(prefix string) error {
	if !strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, "//") {
		return ErrInvalidWorkspacePrefix
	}
	if err := validateCanonicalRelativePrefix(strings.TrimSuffix(prefix, "/"), false); err != nil {
		return err
	}
	return nil
}

// IsUserVisibleWorkspaceObject filters only the implementation marker at the
// exact workspace root. Objects below the prefix, including directory marker
// shaped objects, remain visible.
func IsUserVisibleWorkspaceObject(workspacePrefix, objectKey string) bool {
	return objectKey != workspacePrefix
}

func cloneRootMarkerOptions(options RootMarkerOptions) RootMarkerOptions {
	metadata := make(map[string]string, len(options.Metadata))
	for key, value := range options.Metadata {
		metadata[key] = value
	}
	return RootMarkerOptions{ContentType: options.ContentType, Metadata: metadata}
}

// NewWorkspaceObjectClient constructs a native provider client for FUSE
// control-plane marker operations. It never uses environment credentials.
func NewWorkspaceObjectClient(cfg config.FileSystemConfig, credentials FileSystemCredentials) (WorkspaceObjectClient, error) {
	if len(credentials.AccessKey) == 0 || len(credentials.SecretKey) == 0 {
		return nil, fmt.Errorf("storage: access key and secret key are required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: bucket is required")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("storage: endpoint is required")
	}

	transport, err := newWorkspaceHTTPTransport(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	accessKey := string(credentials.AccessKey)
	secretKey := string(credentials.SecretKey)

	switch StorageProvider(cfg.Provider) {
	case ProviderMinIO:
		client, err := miniogo.New(cfg.Endpoint, &miniogo.Options{
			Creds:        miniocredentials.NewStaticV4(accessKey, secretKey, ""),
			Secure:       cfg.UseSSL,
			Transport:    transport,
			Region:       cfg.Region,
			BucketLookup: miniogo.BucketLookupPath,
			MaxRetries:   1,
		})
		if err != nil {
			return nil, fmt.Errorf("storage: create native MinIO client: %w", err)
		}
		return &minioWorkspaceObjectClient{client: client, bucket: cfg.Bucket, transport: transport}, nil

	case ProviderOBS:
		endpoint, err := validateOBSEndpoint(cfg.Endpoint)
		if err != nil {
			return nil, err
		}
		// OBS accepts its provider-native complete endpoint, including scheme.
		if _, err := obs.New(accessKey, secretKey, endpoint, obs.WithHttpTransport(transport), obs.WithMaxRetryCount(0)); err != nil {
			return nil, fmt.Errorf("storage: create native OBS client: %w", err)
		}
		return &obsWorkspaceObjectClient{
			accessKey: accessKey,
			secretKey: secretKey,
			endpoint:  endpoint,
			bucket:    cfg.Bucket,
			transport: transport,
		}, nil

	default:
		return nil, fmt.Errorf("storage: unsupported workspace object provider %q", cfg.Provider)
	}
}

func validateOBSEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("storage: invalid OBS endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("storage: invalid OBS endpoint: scheme must be http or https")
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("storage: invalid OBS endpoint: absolute URL with host is required")
	}
	if strings.HasPrefix(parsed.Host, "[") || strings.Contains(parsed.Hostname(), ":") {
		return "", fmt.Errorf("storage: invalid OBS endpoint: IPv6 literals are unsupported by the OBS SDK")
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(endpoint, "#") {
		return "", fmt.Errorf("storage: invalid OBS endpoint: userinfo, query, and fragment are forbidden")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("storage: invalid OBS endpoint: path is forbidden")
	}
	if parsed.RawPath != "" || parsed.EscapedPath() != parsed.Path {
		return "", fmt.Errorf("storage: invalid OBS endpoint: escaped path is forbidden")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("storage: invalid OBS endpoint: port is empty")
	}
	if port := parsed.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("storage: invalid OBS endpoint: port must be between 1 and 65535")
		}
	}
	if parsed.Path == "/" {
		return strings.TrimSuffix(endpoint, "/"), nil
	}
	return endpoint, nil
}

func newWorkspaceHTTPTransport(caFile string) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("storage: default HTTP transport has unexpected type")
	}
	transport := base.Clone()

	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("storage: load system certificate roots: %w", err)
	}
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("storage: read custom CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("storage: custom CA file contains no certificates")
		}
	}

	tlsConfig := &tls.Config{RootCAs: roots}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
		tlsConfig.RootCAs = roots
	}
	tlsConfig.InsecureSkipVerify = false
	transport.TLSClientConfig = tlsConfig
	return transport, nil
}

type minioWorkspaceObjectClient struct {
	client    *miniogo.Client
	bucket    string
	transport *http.Transport
}

func (c *minioWorkspaceObjectClient) PutEmptyObject(ctx context.Context, key string, options RootMarkerOptions) error {
	const emptySHA256Hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	core := miniogo.Core{Client: c.client}
	_, err := core.PutObject(ctx, c.bucket, key, bytes.NewReader(nil), 0, "", emptySHA256Hex, miniogo.PutObjectOptions{
		ContentType:          options.ContentType,
		UserMetadata:         cloneStringMap(options.Metadata),
		DisableContentSha256: true, // use the fixed empty hash above, not aws-chunked framing
	})
	return err
}

func (c *minioWorkspaceObjectClient) HeadObject(ctx context.Context, key string) (bool, error) {
	_, err := c.client.StatObject(ctx, c.bucket, key, miniogo.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	response := miniogo.ToErrorResponse(err)
	if isObjectNotFound(response.StatusCode, response.Code) {
		return false, nil
	}
	return false, err
}

func (c *minioWorkspaceObjectClient) DeleteObject(ctx context.Context, key string) error {
	return c.client.RemoveObject(ctx, c.bucket, key, miniogo.RemoveObjectOptions{})
}

type obsWorkspaceObjectClient struct {
	accessKey string
	secretKey string
	endpoint  string
	bucket    string
	transport *http.Transport
}

func (c *obsWorkspaceObjectClient) client(ctx context.Context) (*obs.ObsClient, error) {
	return obs.New(
		c.accessKey,
		c.secretKey,
		c.endpoint,
		obs.WithHttpTransport(c.transport),
		obs.WithMaxRetryCount(0),
		obs.WithRequestContext(ctx),
	)
}

func (c *obsWorkspaceObjectClient) PutEmptyObject(ctx context.Context, key string, options RootMarkerOptions) error {
	client, err := c.client(ctx)
	if err != nil {
		return err
	}
	_, err = client.PutObject(&obs.PutObjectInput{
		PutObjectBasicInput: obs.PutObjectBasicInput{
			ObjectOperationInput: obs.ObjectOperationInput{
				Bucket:   c.bucket,
				Key:      key,
				Metadata: cloneStringMap(options.Metadata),
			},
			HttpHeader:    obs.HttpHeader{ContentType: options.ContentType},
			ContentLength: 0,
		},
		Body: bytes.NewReader(nil),
	})
	return err
}

func (c *obsWorkspaceObjectClient) HeadObject(ctx context.Context, key string) (bool, error) {
	client, err := c.client(ctx)
	if err != nil {
		return false, err
	}
	_, err = client.GetObjectMetadata(&obs.GetObjectMetadataInput{Bucket: c.bucket, Key: key})
	if err == nil {
		return true, nil
	}
	var obsError obs.ObsError
	if errors.As(err, &obsError) && isObjectNotFound(obsError.StatusCode, obsError.Code) {
		return false, nil
	}
	return false, err
}

func (c *obsWorkspaceObjectClient) DeleteObject(ctx context.Context, key string) error {
	client, err := c.client(ctx)
	if err != nil {
		return err
	}
	_, err = client.DeleteObject(&obs.DeleteObjectInput{Bucket: c.bucket, Key: key})
	return err
}

func isObjectNotFound(statusCode int, serviceCode string) bool {
	if serviceCode == "NoSuchKey" || serviceCode == "NotFound" {
		return true
	}
	return statusCode == http.StatusNotFound && serviceCode == ""
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
