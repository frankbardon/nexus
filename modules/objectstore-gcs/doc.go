// Package gcsstore implements objectstore.Backend against Google Cloud
// Storage.
//
// It registers itself as "gcs" from init, so an embedder adds it with a blank
// import and one config key:
//
//	import _ "github.com/frankbardon/nexus/modules/objectstore-gcs"
//
//	core:
//	  object_store:
//	    backend: gcs
//	    bucket: nexus-sessions
//	    prefix: prod/nexus
//
// Nothing in cmd/ imports it. That is the arrangement described in
// docs/src/guides/go-modules.md: the root module's dependency list stays where
// it is, and the Google SDK is carried only by builds that actually want a
// bucket.
//
// # Why this module exists beyond GCS support
//
// This is the *second* independent implementation of objectstore.Backend, and
// it was written to answer a question about the seam rather than about Google:
// is objectstore.Backend a general interface, or is it the shape of the S3 API
// with different names? A second backend that passes objectstoretest.RunSuite
// unmodified, written against a store whose API disagrees with S3 in several
// specific places, is the evidence. It needed no change to
// pkg/engine/objectstore. The list of places GCS genuinely differs, and how
// each was absorbed inside this module, is under "Where GCS differs from S3"
// below -- every one of them turned out to be a translation this module owns,
// not a hole in the interface.
//
// # Why the Google SDK, when the rest of the tree is raw net/http
//
// The same argument doc.go in modules/objectstore-s3 makes, and it lands
// harder here.
//
// The data plane is small: the JSON API's object insert, get, delete and list
// are four endpoints. If that were the whole job, hand-rolling would have won
// on house-style grounds alone.
//
// The credential chain is the job, and Google's is broader than AWS's.
// Application Default Credentials covers GOOGLE_APPLICATION_CREDENTIALS, the
// gcloud well-known file, GKE Workload Identity and GCE service accounts via
// the metadata server (with expiry-aware refresh), service-account
// impersonation, and Workload Identity Federation -- which is an STS token
// exchange against an external OIDC or AWS identity followed by a second
// impersonation hop. Every one of those is a path no emulator reproduces, so a
// hand-rolled version would ship its riskiest code untested.
//
// There is a second reason the SDK earns its place here that has no S3
// equivalent: the client validates the CRC32C the service reports against the
// bytes it uploaded or downloaded, and refuses the object on a mismatch. That
// is real end-to-end integrity checking for artifacts the engine treats as the
// durable copy of a session, and reimplementing it correctly -- including the
// resumable-upload protocol it interacts with -- is not a few hundred lines.
//
// So: cloud.google.com/go/storage, used narrowly. The client, the option
// package and the iterator, plus one credential-detection call. The rejected
// alternative was hand-rolled net/http against the JSON API, which would have
// matched house style at the cost of reimplementing ADC and checksum
// validation.
//
// # Deliberately synchronous
//
// objectstore.Backend permits Put and Delete to complete asynchronously, and
// this backend does not take that permission, for the reasons the S3 module
// records: the engine already owns retry, backoff and the degrade/strict
// failure policy, and a queue in here would put a second retry regime
// underneath it. Put returns when GCS has acknowledged and checksummed the
// object; Flush therefore has nothing to wait for.
//
// # Where GCS differs from S3
//
// Every one of these is handled inside this module. None of them needed a
// change to the seam.
//
//   - Deleting a missing object is an ERROR on GCS (storage.ErrObjectNotExist)
//     where S3 reports success. objectstore.Backend specifies the S3
//     behaviour, so Delete swallows exactly that sentinel. See Delete.
//
//   - There is no region. A GCS bucket has a location, but it is a property of
//     the bucket, fixed when it was created, and the client never names one --
//     unlike S3, where the region is signed into every request. core.object_store.region
//     is therefore accepted and ignored, with a warning at boot rather than a
//     failure, because a shared config that names a region for a colleague's S3
//     deployment should not stop this one from starting. See New.
//
//   - There is no project ID either, which is worth stating because most GCS
//     examples begin with one. A project is needed to create or list buckets;
//     this backend does neither. It reads, writes, deletes and lists objects
//     inside a bucket that already exists, and every one of those is addressed
//     by bucket name alone.
//
//   - endpoint means something narrower. On S3 it addresses a whole family of
//     independently operated, genuinely authenticated stores -- MinIO, R2,
//     Ceph, Backblaze. GCS has exactly one production service, and it is
//     reached by leaving endpoint empty; a VPC using Private Google Access or
//     Private Service Connect gets there by DNS and routing policy, not by a
//     client-side endpoint override. So the only thing anyone points this
//     backend at with endpoint is an emulator. See normalizeEndpoint and
//     resolveCredentials.
//
//   - Uploads are not retried by default, because an object insert without a
//     precondition is not idempotent as far as the SDK is concerned. This
//     backend always writes whole objects and takes last-write-wins, so a
//     retried upload is safe by construction; RetryAlways is set on the write
//     path to get behaviour equivalent to the AWS SDK's default. See object.
//
//   - The default upload chunk size allocates 16 MiB per writer. A session tree
//     is hundreds of small files, so that allocation is paid per artifact for
//     no benefit. Put sizes the chunk from the file. See chunkSizeFor.
//
//   - The client holds resources worth releasing, which the S3 one does not.
//     Backend therefore implements io.Closer; the engine type-asserts for it
//     rather than the interface requiring it, which is why no seam change was
//     needed here either. See Close.
package gcsstore
