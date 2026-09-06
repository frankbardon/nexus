package s3store

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// BackendName is the name this backend registers under, and the value an
// operator writes for core.object_store.backend.
const BackendName = "s3"

// fallbackRegion is used when a custom endpoint is configured and no region
// was given.
//
// SigV4 signs over a region whether or not the store has any concept of one,
// so the signer needs a non-empty string. MinIO, Ceph and Backblaze ignore it
// entirely, and "us-east-1" is the value every S3-compatible store is set up to
// accept because it is what the AWS tooling defaults to. Guessing here rather
// than making region mandatory is deliberate: requiring an operator running
// MinIO on a laptop to invent a region would be asking them to fill in a field
// that means nothing to their deployment.
//
// No fallback is applied when there is no custom endpoint. Against real AWS the
// region selects the data's physical location, and silently defaulting it would
// put a bucket-not-found in front of an operator whose bucket exists in the
// region they forgot to name.
const fallbackRegion = "us-east-1"

func init() {
	// Driver-style registration, per objectstore.Register. This is the entire
	// coupling between this module and Nexus core: a blank import turns
	// `backend: s3` from a boot failure into a working configuration, and core
	// never learns this module exists.
	objectstore.Register(BackendName, New)
}

// New builds an S3 backend from a validated object-store config. It is the
// objectstore.Factory registered above, exported so an embedder who wants to
// construct one directly -- for a test, or to run two backends side by side --
// does not have to go through the registry.
//
// # What is and is not checked here
//
// Everything cheap and local is checked: the endpoint parses, a credentials
// file that was named exists, a region is resolvable. Those are configuration
// mistakes, and house style is that a malformed configuration fails the boot
// rather than surfacing an hour into a session.
//
// Nothing remote is checked. A HeadBucket probe was considered and rejected:
// core.object_store.failure_policy exists so that an object-store outage
// degrades a run instead of ending it, and a boot-time round trip would make
// `degrade` structurally unable to degrade -- a five-minute S3 blip would stop
// every agent from starting, which is strictly worse than the behaviour the
// policy promises. Credential retrieval is left lazy for the same reason: under
// IRSA and IMDS it is itself a network call, and the SDK refreshes on expiry
// anyway, so resolving once at boot would prove less than it appears to.
func New(ctx context.Context, cfg objectstore.Config) (objectstore.Backend, error) {
	logger := cfg.Logger
	if logger == nil {
		// Config documents Logger as engine-injected and possibly nil for a
		// hand-built Config, so nil-check rather than assume.
		logger = slog.Default()
	}

	if cfg.Bucket == "" {
		// Config.Validate already covers this for the config path; repeat it
		// for a direct caller, who has not necessarily been through Validate.
		return nil, fmt.Errorf("s3 object store: bucket is required")
	}
	if err := objectstore.ValidateKeyPrefix(cfg.Prefix); err != nil {
		return nil, fmt.Errorf("s3 object store: prefix: %w", err)
	}
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}

	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(awsCfg, clientOptions(cfg)...)

	logger.Debug("s3 object store opened",
		"bucket", cfg.Bucket,
		"prefix", cfg.Prefix,
		"region", awsCfg.Region,
		"endpoint", cfg.Endpoint,
		"path_style", cfg.Endpoint != "",
		"credentials", credentialSourceLabel(cfg))

	return &Backend{
		api:    client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
		log:    logger,
	}, nil
}

// loadAWSConfig resolves credentials and region.
//
// Both supported credential sources go through config.LoadDefaultConfig, which
// is the whole reason this module takes the SDK (see doc.go):
//
//   - Ambient (credentials_file empty) is the production path. The SDK's
//     default chain covers environment variables, the shared config and
//     credentials files, IRSA / EKS Pod Identity via the projected
//     web-identity token, ECS task roles and the EC2 instance role via IMDSv2 --
//     in that order, with expiry-aware refresh. Nexus does not reorder or
//     restrict the chain; an operator who has made the AWS CLI work on a host
//     has made this work.
//
//   - Static (credentials_file set) pins the shared *credentials* file to the
//     named path. The file is the ordinary AWS INI format, so the same file
//     works with the AWS CLI and can be mounted as a Kubernetes secret without
//     translation; AWS_PROFILE still selects the profile, defaulting to
//     "default". Inventing a Nexus-specific two-line key/secret format was
//     rejected -- it would have been marginally simpler to write and would have
//     broken every existing tool for producing one.
//
// Note that only the credentials file is pinned, not the shared *config* file:
// the two are separate files with separate roles in AWS tooling, and pointing
// `credentials_file` at a config file's worth of settings would let a
// credentials path silently change the region and endpoint too.
func loadAWSConfig(ctx context.Context, cfg objectstore.Config) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}

	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	if cfg.CredentialsFile != "" {
		// The path arrives already expanded: objectstore.Config documents that
		// the engine runs it through engine.ExpandPath at config load, and this
		// module must not re-expand it (nor could it -- ExpandPath lives in
		// package engine, which the seam is deliberately importable without).
		//
		// Stat it rather than letting the SDK discover it: a shared credentials
		// file that does not exist is *ignored* by the SDK, which then falls
		// through to ambient credentials. An operator who typo'd the path would
		// get a working process authenticating as the wrong principal, or a
		// confusing "no credentials" much later. Fail the boot instead.
		if _, err := os.Stat(cfg.CredentialsFile); err != nil {
			return aws.Config{}, fmt.Errorf("s3 object store: credentials_file %q: %w", cfg.CredentialsFile, err)
		}
		opts = append(opts, awsconfig.WithSharedCredentialsFiles([]string{cfg.CredentialsFile}))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("s3 object store: loading AWS configuration: %w", err)
	}

	if awsCfg.Region == "" {
		if cfg.Endpoint == "" {
			return aws.Config{}, fmt.Errorf(
				"s3 object store: no region resolved; set core.object_store.region or AWS_REGION")
		}
		awsCfg.Region = fallbackRegion
	}
	return awsCfg, nil
}

// clientOptions turns the endpoint half of the config into S3 client options.
func clientOptions(cfg objectstore.Config) []func(*s3.Options) {
	if cfg.Endpoint == "" {
		return nil
	}
	return []func(*s3.Options){func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)

		// Path-style addressing (https://host/bucket/key) is forced whenever a
		// custom endpoint is set, and that is the single line that makes MinIO,
		// Ceph and Backblaze work unmodified. Virtual-host addressing puts the
		// bucket in the hostname, which needs a wildcard DNS record and a
		// wildcard TLS certificate that a self-hosted store or a laptop
		// container does not have.
		//
		// Exposing this as its own config key was rejected: it would mean
		// adding a field to objectstore.Config, which is core's shared struct
		// for every backend, to express something only this one has an opinion
		// about -- and no store in the target set (MinIO, R2, Ceph, Backblaze)
		// requires virtual-host addressing. Real AWS, which prefers it, is
		// exactly the case where no endpoint is set and this block does not
		// run.
		o.UsePathStyle = true

		// Ask for a checksum only where the operation requires one.
		//
		// The SDK's default (WhenSupported) adds a trailing CRC32 to every
		// PutObject, which switches the request body to aws-chunked transfer
		// encoding. Several S3-compatible stores reject or mis-handle that, and
		// the failure is a signature mismatch that says nothing about
		// checksums. Nothing is lost: the request is still SigV4-signed over
		// its payload hash, so corruption in flight is detected either way.
		//
		// Scoped to the custom-endpoint case on purpose -- against real AWS the
		// default is well supported and is the better setting.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}}
}

// credentialSourceLabel names the credential source for the boot log, so an
// operator debugging a permissions error can see which of the two paths was
// taken without turning on SDK tracing.
func credentialSourceLabel(cfg objectstore.Config) string {
	if cfg.CredentialsFile != "" {
		return "file:" + cfg.CredentialsFile
	}
	return "ambient"
}

// validateEndpoint holds the rule that an endpoint must be an absolute http(s)
// URL. Called from New, before anything else looks at the endpoint.
//
// The SDK accepts a bare "localhost:9000" and then fails much later with an
// unresolvable-host error that does not mention the config key, so catching it
// here is the difference between a boot failure naming core.object_store.endpoint
// and a mystery at first write.
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("s3 object store: endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("s3 object store: endpoint %q must be an absolute http:// or https:// URL", endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("s3 object store: endpoint %q has no host", endpoint)
	}
	return nil
}
