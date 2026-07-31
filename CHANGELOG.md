Unreleased
==========

  * add `/livez` and GCS-backed `/readyz` health-check endpoints
  * add a Nix flake with a package, development shell, formatter, and checks
  * update to Go 1.26.5 and refresh all Go dependencies, including urfave/cli v3
  * replace Negroni with the standard library HTTP server
  * add golangci-lint and strengthen checks for error handling, error wrapping,
    context propagation, and resource cleanup
  * harden the HTTP server with timeouts, signal handling, graceful shutdown,
    and wrapped lifecycle errors
  * preserve GCS response metadata and validators, and support byte-range and
    conditional cache reads
  * avoid a separate GCS metadata request when an `If-Range` validator matches
  * reduce memory used by concurrent small uploads while retaining resumable
    16 MiB chunks for large and unknown-size objects
  * abort failed streaming uploads instead of finalizing partial GCS objects
  * coalesce bursts of GCS-backed readiness checks with a one-second result
    cache
  * modernize the Nix build and document current flake, upload, cache-consumer,
    health-check, and localhost-first performance workflows


0.1.0 / 2019-09-04
==================

  * client: use ScopeReadWrite, not ScopeFullControl (#5)
  * PUT: write metadata while creating object, not immediately afterwards (#6)


0.0.2 / 2019-09-02
==================

  * improve http response statuses
  * log errors to console (#4)

0.0.1 / once upon a time
========================

Initial release!
