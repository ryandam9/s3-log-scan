package scan

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3API is the slice of the S3 client the engine uses; the stub server
// in tests implements the same interface.
type S3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}
