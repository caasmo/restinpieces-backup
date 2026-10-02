// Package s3 holds the pieces the S3 upload and download features share.
package s3

import (
	"errors"

	"github.com/caasmo/restinpieces/config"
	s3client "github.com/caasmo/restinpieces/s3"
)

// NewClient builds an S3 client from the s3 section. It returns an error
// when the endpoint is empty, in which case the feature is deactivated and
// its tick or job is skipped.
func NewClient(s3Config config.S3) (*s3client.S3, error) {
	if s3Config.Endpoint == "" {
		return nil, errors.New("s3.endpoint is not configured")
	}

	return &s3client.S3{
		Endpoint:             s3Config.Endpoint,
		Region:               s3Config.Region,
		AccessKey:            s3Config.AccessKey,
		SecretKey:            s3Config.SecretKey,
		UsePathStyle:         s3Config.UsePathStyle,
		RequireContentLength: s3Config.RequireContentLength,
	}, nil
}
