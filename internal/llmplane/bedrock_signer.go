package llmplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// sharedSigner is a stateless SigV4 signer reused for every mode.
var sharedSigner = v4.NewSigner()

// signRequest SigV4-signs req in place with the given credentials provider. req.URL.Host
// must already be the resolved bedrock-runtime host (Host is part of the signature).
// service is "bedrock" (NOT bedrock-runtime); payload hash is sha256(body).
func signRequest(ctx context.Context, req *http.Request, body []byte, region string, provider aws.CredentialsProvider) error {
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	return sharedSigner.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock", region, time.Now())
}

// staticProvider wraps client-supplied raw AWS credentials (Flow B).
func staticProvider(accessKeyID, secretAccessKey, sessionToken string) aws.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)
}

// sessionName is the STS RoleSessionName for a tenant: "gw-" + tenant, limited to
// the characters and 64-byte length STS accepts.
func sessionName(tenant string) string {
	b := []byte("gw-")
	for i := 0; i < len(tenant) && len(b) < 64; i++ {
		if c := tenant[i]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("+=,.@_-", c) >= 0 {
			b = append(b, c)
		}
	}
	return string(b)
}

// maxRoleProviders bounds the assumed-role cache; when full, one random entry is
// evicted per insert. ponytail: random rather than LRU eviction.
const maxRoleProviders = 1024

// bedrockSigner holds the gateway's OWN AWS identity (Flow C) and the assumed-role
// providers built from it (Flow B, role variant). The default credential chain is
// loaded once; credentials are Retrieved lazily per request so expiring SSO/STS
// creds refresh without restarting the gateway.
type bedrockSigner struct {
	mu     sync.Mutex
	cfg    aws.Config
	loaded bool
	roles  map[string]aws.CredentialsProvider // roleARN|externalID|region → cached assumed-role creds
}

// baseConfig returns the gateway's own AWS config, loaded once.
func (s *bedrockSigner) baseConfig(ctx context.Context) (aws.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return aws.Config{}, err
		}
		s.cfg, s.loaded = cfg, true
	}
	return s.cfg, nil
}

// gatewayProvider returns the gateway's own credentials provider (Flow C).
func (s *bedrockSigner) gatewayProvider(ctx context.Context) (aws.CredentialsProvider, error) {
	cfg, err := s.baseConfig(ctx)
	if err != nil {
		return nil, err
	}
	return cfg.Credentials, nil
}

// roleProvider returns a cached provider that assumes roleARN (with externalID)
// via STS using the gateway's base credentials. The cache means one AssumeRole
// per credential lifetime instead of one per request; aws.CredentialsCache
// refreshes a minute before expiry and never caches a failed Retrieve.
func (s *bedrockSigner) roleProvider(ctx context.Context, roleARN, externalID, region string) (aws.CredentialsProvider, error) {
	cfg, err := s.baseConfig(ctx)
	if err != nil {
		return nil, err
	}
	key := roleARN + "|" + externalID + "|" + region
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.roles[key]; ok {
		return p, nil
	}
	if s.roles == nil {
		s.roles = make(map[string]aws.CredentialsProvider)
	}
	for k := range s.roles { // full: evict one (map order is random), never everyone
		if len(s.roles) < maxRoleProviders {
			break
		}
		delete(s.roles, k)
	}
	stsClient := sts.NewFromConfig(cfg, func(o *sts.Options) { o.Region = region })
	p := aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String(externalID)
		o.RoleSessionName = sessionName(externalID) // tenant shows in the role account's CloudTrail
	}), func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = time.Minute // refresh before STS expiry, not on the first failed call
		o.ExpiryWindowJitterFrac = 0.2
	})
	s.roles[key] = p
	return p, nil
}

// forgetRole drops a cached role provider whose AssumeRole failed, so a
// nonexistent or untrusted role never holds a cache slot.
func (s *bedrockSigner) forgetRole(roleARN, externalID, region string) {
	s.mu.Lock()
	delete(s.roles, roleARN+"|"+externalID+"|"+region)
	s.mu.Unlock()
}
