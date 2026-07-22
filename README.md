# s3logscan

A resource-budgeted concurrent scanner for EMR/YARN logs stored in S3.

`s3logscan` scans an S3 prefix and reports object keys and matching content
lines, grep-style. It rejects as much work as possible before any download
using listing metadata, streams `.gz`/`.bz2`/plain objects, handles `.zip`
under explicit disk and expansion budgets, and reports the YARN application
IDs behind matches — even when the ID is absent from the object key (step
logs). See [docs/design.md](docs/design.md) for the full design.

## Install

```
go install github.com/ryandam9/s3-log-scan/cmd/s3logscan@latest
```

or build from a checkout:

```
go build ./cmd/s3logscan
```

## Usage

Known application — locate and read every log object belonging to a YARN
application ID:

```
s3logscan -bucket my-emr-logs \
  -prefix logs/j-1ABC2DEF3GHI4/ \
  -key application_1700000000000_0042 \
  -grep 'ERROR|Exception'
```

Unknown application — discover which application produced an error, then
drill in. Step logs are tiny and usually carry the failure summary, so
smallest-first finds them quickly:

```
s3logscan -bucket my-emr-logs \
  -prefix logs/j-1ABC2DEF3GHI4/ \
  -grep 'Table or view not found' -F \
  -l -discover-apps -smallest-first
```

The run summary prints the deduplicated set of application IDs discovered
from object keys, matching lines, and preceding context.

List-only mode (no downloads) — omit `-grep`:

```
s3logscan -bucket my-emr-logs -prefix logs/j-1ABC2DEF3GHI4/steps/
```

### Flags

```
-bucket string                  required
-prefix string                  required unless -allow-whole-bucket-scan
-allow-whole-bucket-scan        explicit opt-in to empty-prefix enumeration
-key pattern                    object-key filter (client-side; cuts GETs, not LIST work)
-grep pattern                   content filter; omit for list-only mode (no downloads)
-F                              -key/-grep are fixed strings, not regex
-i                              case-insensitive matching
-ext list                       e.g. .gz,.log (case-insensitive)
-after / -before time           RFC3339 or YYYY-MM-DD (UTC midnight);
                                after inclusive, before exclusive; vs LastModified
-max-size MiB                   compressed object cap (default 128; 0 = unlimited)
-max-zip-entries N              default 10000
-max-uncompressed-object-size MiB   cumulative ZIP expansion budget (default 512)
-max-line-size MiB              oversized-line truncation boundary (default 4)
-max-matches N                  per object; whole ZIP = one object (0 = unlimited)
-l                              names only; first-hit exit; best-effort IDs
-discover-apps                  with -l: read on until an application ID is found
-smallest-first                 windowed approximate size ordering
-smallest-first-window N        default 5000
-workers N                      1-256 (default 16)
-zip-workers N                  1-workers (default 2)
-object-timeout / -overall-timeout   durations (0 = none)
-request-payer requester        requester-pays buckets
-expected-bucket-owner id       cross-account safety check
-sanitize-output bool           default true
-max-warnings N                 default 100
-region string                  AWS region override
```

Regex patterns use Go RE2 semantics: no lookaround, no backreferences.
Use `-F` for fixed strings.

### What filtering does and does not save

The prefix is the **only server-side filter**. `-key`, `-ext`, `-max-size`,
and the time window run client-side against listing pages: they cut
`GetObject` calls, downloaded bytes, and decompression work — often by
orders of magnitude — but listing time remains proportional to the number
of keys under the prefix. Always scope the prefix to a cluster
(`.../j-<CLUSTER>/`) where you can. Whole-bucket enumeration requires
`-allow-whole-bucket-scan`.

### Output

Matches go to stdout in completion order (non-deterministic under
concurrency):

```
s3://bucket/key:lineNo: text
s3://bucket/key!zipEntry:lineNo: text
```

Output is sanitized by default (`-sanitize-output=false` to disable):
C0/C1 control characters, DEL, invalid UTF-8 bytes, and deceptive Unicode
formatting (bidirectional overrides and isolates, zero-width characters,
line/paragraph separators, BOM) in content, keys, and ZIP entry names are
each replaced with `?` — tab is kept. Diagnostics go to stderr, capped by
`-max-warnings`. A summary is always printed to stderr — including after
interruption — separating objects scanned to EOF, stopped early by
request (`-l`, `-max-matches`), partially scanned, skipped (with
reasons), and failed, plus per-class error counts and discovered
application IDs.

### Exit codes

```
0    completed; one or more matches; no object errors or partial scans
1    completed; no matches
2    fatal usage, configuration, credential, or listing error
3    completed, but with >=1 object error or partially scanned object;
     also used when -overall-timeout expires or stdout fails mid-run
130  interrupted (SIGINT/SIGTERM); summary is still printed
```

`-h`/`-help` and `-version` exit 0. `-overall-timeout` deliberately maps
to 3, not 130: a configured deadline is a partial run, not an external
interruption, and automation can tell the two apart. An object cut off
by `-object-timeout` is counted as partially scanned with a `timeout`
error — never as fully scanned.

A *partially scanned* object means content may have been missed: an
oversized line was truncated, a ZIP budget aborted processing, or a stream
failed mid-download. Partial scans are never silent — they show in the
summary and in the exit status.

## Resource budgets

Every resource is bounded by an explicit, independently configurable
budget — worker count alone bounds none of the ones that matter:

| Resource | Bound by | Default |
|---|---|---|
| Download parallelism / open response bodies | `-workers` | 16 |
| RAM per worker | `-max-line-size` + fixed read buffers | ~4.3 MiB × workers |
| ZIP concurrency | `-zip-workers` | 2 |
| Temp disk | `zip-workers × -max-size` | 256 MiB |
| Archive expansion | `-max-uncompressed-object-size` (cumulative) | 512 MiB per ZIP |
| ZIP entry count | `-max-zip-entries` | 10 000 |
| Scheduler memory | `-smallest-first-window` descriptors | 5 000 |
| Queued output | bounded result channel | workers × 8 lines |
| stderr volume | `-max-warnings` | 100 |
| Wall clock | `-object-timeout`, `-overall-timeout` | off |

## AWS setup

Credentials resolve through the standard chain (environment variables,
`AWS_PROFILE`, instance/task roles). Least-privilege IAM policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-emr-logs",
      "Condition": { "StringLike": { "s3:prefix": "logs/*" } }
    },
    {
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::my-emr-logs/logs/*"
    }
  ]
}
```

When the bucket uses SSE-KMS, also grant `kms:Decrypt` on the bucket key.
KMS denials are reported under the `accessDenied` error class.

- **Cross-region buckets work automatically.** When `-region` is not
  given, the bucket's region is discovered up front (via `HeadBucket`,
  whose response names the bucket region even on wrong-region or
  access-denied probes) and used for all requests — so a bucket in,
  say, `ap-southeast-4` scans correctly from a client configured for
  another region. An explicit `-region` always wins and skips the
  probe; if a region mismatch still surfaces, the listing error says
  to rerun with `-region <bucket-region>`.
- `-expected-bucket-owner <account-id>` guards against cross-account
  bucket confusion.
- `-request-payer requester` supports requester-pays buckets.
- Archive handling is restore-aware: listing requests the
  `RestoreStatus` attribute, so a Glacier/Deep Archive object with a
  readable restored copy **is scanned**; unrestored (or
  restore-in-progress) objects are skipped at listing time and counted
  (`archivedSkipped`). Glacier Instant Retrieval objects are scanned
  normally. Objects in the Intelligent-Tiering archive tiers keep a
  readable-looking storage class; their `InvalidObjectState` GET
  failures are classified as `archivedUnavailable`.
- Error classes in the summary: `accessDenied` (including KMS),
  `notFound`, `changedAfterListing`, `corrupt`, `timeout`,
  `archivedUnavailable`, `throttled` (SlowDown et al.), `other`.

## Behavior guarantees

- **LIST/GET consistency**: every `GetObject` carries `If-Match` with the
  listed ETag. An object that changed in between is counted
  (`changedAfterListing`) and skipped — metadata and content are never
  mixed across versions.
- **No retry after output**: once scanning has begun, a mid-stream failure
  marks the object partially scanned and is never retried, so matches are
  never duplicated.
- **Folder markers**: only keys ending in `/` **and** zero bytes are
  skipped as markers; non-empty `/`-suffixed objects are scanned.
- **Broken pipes**: if stdout fails (`| head`, closed redirect), the run
  cancels promptly and reports interruption instead of a misleading
  success summary.

## Application-ID attribution

Attribution is per match, not per object: at each matching line the ID
is resolved from (in priority order) the object key, the matching line
itself, or the most recent preceding ID in the same stream. One object
can therefore contribute several application IDs, and an unrelated ID
appearing after a match is never attributed to it. Each ZIP entry gets
its own context, so IDs cannot leak between entries (the outer key's ID,
when present, applies to all entries). With `-l -discover-apps`, reading
continues silently after the first match until an ID appears or the
object ends.

## Development

```
make            # fmt + vet + race tests + build + install
go test -race ./...
```

CI (GitHub Actions) runs gofmt, `go mod tidy` drift check, `go vet`,
the race-enabled test suite, a build, and `govulncheck` on every push
and pull request.

The test suite covers every filter boundary, oversized-line and exact
line-limit behavior, truncated streams, ZIP budgets (including the
exact-expansion boundary), gzip multistream, per-match application-ID
attribution and ZIP-entry isolation, object/overall timeout accounting,
restore-aware archive filtering, pagination boundaries, LIST/GET races
(`If-Match`), error classification, warning suppression, cancellation,
and broken-pipe handling — against a stub S3, with the race detector
clean, plus fuzz targets for the line iterator, sanitizer, and ID
extraction.
