package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// SMClient is the minimal interface for AWS Secrets Manager GetSecretValue.
// The production implementation uses the AWS SDK v2; tests inject a stub.
type SMClient interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// AWSSMSResolver reads credentials from AWS Secrets Manager. The logical
// NEXUS_* env-var name is mapped to a Secrets Manager secret ID via the
// configured prefix: the env var name itself (e.g. NEXUS_FRONTIER_API_KEY)
// becomes "{prefix}/NEXUS_FRONTIER_API_KEY" (or just the name when no
// prefix is set). The secret's plaintext is expected to be the raw string
// value. A missing secret falls back to os.Getenv.
type AWSSMSResolver struct {
	client SMClient
	prefix string
	cache  map[string]string
}

// NewAWSSMSResolver creates a Resolver backed by AWS Secrets Manager.
// It uses the default AWS credential chain (env vars → shared creds →
// EC2/ECS role). When cfg.HTTPClient is provided (recommended), the
// client is guarded by EgressGuard to prevent SSRF to private IP ranges
// (issue #1411). Fail-closed: if the SDK client cannot be initialised,
// NewAWSSMSResolver returns an error.
func NewAWSSMSResolver(cfg BackendConfig) (Resolver, error) {
	ctx := context.Background()
	var awsCfg aws.Config
	var err error
	if cfg.HTTPClient != nil {
		awsCfg, err = awscfg.LoadDefaultConfig(ctx, awscfg.WithHTTPClient(cfg.HTTPClient))
	} else {
		awsCfg, err = awscfg.LoadDefaultConfig(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: aws config: %w", err)
	}

	client := secretsmanager.NewFromConfig(awsCfg)

	return &AWSSMSResolver{
		client: client,
		prefix: cfg.AWSSMPrefix,
		cache:  make(map[string]string),
	}, nil
}

func (r *AWSSMSResolver) secretID(name string) string {
	if r.prefix == "" {
		return name
	}
	return r.prefix + "/" + name
}

// Resolve looks up name in AWS Secrets Manager. If the secret is absent
// (ResourceNotFoundException) it falls back to os.Getenv.
func (r *AWSSMSResolver) Resolve(name string) (string, error) {
	if v, ok := r.cache[name]; ok {
		return v, nil
	}

	id := r.secretID(name)
	resp, err := r.client.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{
		SecretId: &id,
	})

	if err != nil {
		var nf *types.ResourceNotFoundException
		if errors.As(err, &nf) {
			return os.Getenv(name), nil
		}
		return "", fmt.Errorf("awssm get %q: %w", name, err)
	}

	if resp != nil && resp.SecretString != nil {
		val := *resp.SecretString
		r.cache[name] = val
		return val, nil
	}

	return os.Getenv(name), nil
}

// --- testing helpers ---

// newAWSSMSResolverWithClient constructs an AWSSMSResolver with a custom
// client. Used by tests to inject a stub without a live AWS connection.
func newAWSSMSResolverWithClient(client SMClient, prefix string) *AWSSMSResolver {
	return &AWSSMSResolver{
		client: client,
		prefix: prefix,
		cache:  make(map[string]string),
	}
}

// stubSMClient is a test double for SMClient.
type stubSMClient struct {
	values map[string]string
	err    error
	calls  int
}

func (s *stubSMClient) GetSecretValue(_ context.Context, params *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	id := ""
	if params.SecretId != nil {
		id = *params.SecretId
	}
	val, ok := s.values[id]
	if !ok {
		return nil, &types.ResourceNotFoundException{}
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: &val}, nil
}
