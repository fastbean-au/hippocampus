package kms

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
)

// awsContextKey is the encryption-context key the additional data travels under. AWS KMS binds a
// ciphertext to its encryption context exactly as an AEAD binds one to its additional data, and
// records it in CloudTrail beside every Decrypt, which is what an auditor reads.
const awsContextKey = "hippocampus_data_key"

// awsWrapper is AWS KMS. Credentials and, failing a region in the key's ARN, the region come from
// the SDK's default chain - the environment, the shared config, or the instance role.
type awsWrapper struct {
	key    string
	client *awskms.Client
}

// newAWS builds the AWS KMS client.
func newAWS(ctx context.Context, cfg Config) (*awsWrapper, error) {
	var opts []func(*awsconfig.LoadOptions) error

	// A key ARN names its region; an alias or a bare key id does not, and leaves it to the chain.
	if region := awsRegionFromARN(cfg.Key); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading the AWS configuration for KMS: %w", err)
	}

	client := awskms.NewFromConfig(awsCfg, func(o *awskms.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})

	return &awsWrapper{key: cfg.Key, client: client}, nil
}

// awsRegionFromARN returns the region of a KMS key or alias ARN (arn:aws:kms:<region>:…), or "".
func awsRegionFromARN(key string) string {
	parts := strings.Split(key, ":")

	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "kms" {
		return ""
	}

	return parts[3]
}

// Provider implements Wrapper.
func (w *awsWrapper) Provider() string {
	return "aws"
}

// Key implements Wrapper.
func (w *awsWrapper) Key() string {
	return w.key
}

// Wrap implements Wrapper.
func (w *awsWrapper) Wrap(ctx context.Context, dataKey []byte, aad []byte) ([]byte, error) {
	out, err := w.client.Encrypt(ctx, &awskms.EncryptInput{
		KeyId:             aws.String(w.key),
		Plaintext:         dataKey,
		EncryptionContext: map[string]string{awsContextKey: string(aad)},
	})
	if err != nil {
		return nil, fmt.Errorf("AWS KMS Encrypt under %s: %w", w.key, err)
	}

	return out.CiphertextBlob, nil
}

// Unwrap implements Wrapper. The key is passed even though a symmetric ciphertext names its own:
// AWS then refuses one wrapped under any other key, rather than decrypting whatever it is handed.
func (w *awsWrapper) Unwrap(ctx context.Context, wrapped []byte, key string, aad []byte) ([]byte, error) {
	out, err := w.client.Decrypt(ctx, &awskms.DecryptInput{
		KeyId:             aws.String(key),
		CiphertextBlob:    wrapped,
		EncryptionContext: map[string]string{awsContextKey: string(aad)},
	})
	if err != nil {
		return nil, fmt.Errorf("AWS KMS Decrypt under %s: %w: %w", key, ErrUnwrap, err)
	}

	if len(out.Plaintext) == 0 {
		return nil, fmt.Errorf("AWS KMS Decrypt under %s: %w: the reply holds no plaintext", key, ErrUnwrap)
	}

	return out.Plaintext, nil
}
