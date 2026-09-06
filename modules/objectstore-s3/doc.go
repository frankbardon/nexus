// Package s3store implements objectstore.Backend against Amazon S3 and any
// S3-compatible store: MinIO, Cloudflare R2, Ceph RGW, Backblaze B2.
//
// It registers itself as "s3" from init, so an embedder adds it with a blank
// import and one config key:
//
//	import _ "github.com/frankbardon/nexus/modules/objectstore-s3"
//
//	core:
//	  object_store:
//	    backend: s3
//	    bucket: nexus-sessions
//	    prefix: prod/nexus
//	    region: us-east-1
//
// Nothing in cmd/ imports it. That is the arrangement described in
// docs/src/guides/go-modules.md: the root module's dependency list stays where
// it is, and the AWS SDK is carried only by builds that actually want a bucket.
//
// # Why the AWS SDK, when the rest of the tree is raw net/http
//
// House style is deliberate about dependencies -- every LLM provider, the
// broker and the Kubernetes client are hand-rolled net/http. Hand-rolling this
// backend was seriously considered on that precedent and rejected.
//
// The S3 data plane itself is not the hard part. GET, PUT, DELETE and
// ListObjectsV2 over XML are a few hundred lines, and SigV4, while fiddly, is
// exactly specified and testable against published vectors. If signing were the
// whole job, hand-rolling would have won.
//
// The credential chain is the job. This backend must work under IRSA (a
// projected web-identity token file that rotates on its own schedule, exchanged
// via AssumeRoleWithWebIdentity, with the resulting session credentials
// refreshed before expiry), under an EC2 instance role (IMDSv2's PUT-a-token
// dance, hop-limit quirks, and the same refresh problem), under ECS task roles
// (a third endpoint with a fourth auth shape), and under plain environment or
// shared-file credentials. That is four independent providers plus expiry
// management, and it is precisely the part no emulator reproduces -- see the
// PRD's emulator-fidelity risk. A hand-rolled version of it would be code whose
// first real test is a production cluster.
//
// The LLM-provider analogy does not carry: those authenticate with a static
// bearer header read once from the environment. There is no chain and nothing
// refreshes. The comparison that does carry is the reason submodules exist at
// all -- a backend module may take a dependency the root module must not, and
// this is the dependency it was invented for.
//
// So: github.com/aws/aws-sdk-go-v2, and specifically config.LoadDefaultConfig
// for credentials. The rejected alternative was hand-rolled net/http + SigV4,
// which would have matched house style at the cost of reimplementing the
// credential chain, and would have shipped its riskiest path untested.
//
// The SDK is used narrowly. Only the s3 client and the config/credentials
// packages are imported; no S3 transfer manager, no higher-level helper. See
// the note on Put for why multipart upload was left out.
//
// # Deliberately synchronous
//
// objectstore.Backend permits Put and Delete to complete asynchronously, and
// this backend does not take that permission. Both issue their request inline
// and return when the store has acknowledged it.
//
// The queue-and-batch design was rejected here because the durability it would
// buy already exists one layer up: the engine owns retry, backoff and the
// degrade/strict failure policy, and duplicating a queue inside the backend
// would give the same object two independent retry regimes with no way to
// reason about the order they interleave. A synchronous backend is also the
// only shape under which the contract suite's read-after-write cases mean
// anything -- and it makes Flush's durability promise trivially true rather
// than something to argue about. See Flush.
package s3store
