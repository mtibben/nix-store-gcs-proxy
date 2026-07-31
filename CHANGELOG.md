# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Add `/livez` and GCS-backed `/readyz` health-check endpoints.
- Add a Nix flake with a package, development shell, formatter, and checks.
- Add golangci-lint checks for error handling, error wrapping, context
  propagation, and resource cleanup.
- Add support for byte-range and conditional cache reads.

### Changed

- Update to Go 1.26.5 and refresh all Go dependencies, including urfave/cli v3.
- Derive clean Nix flake build versions from the Git revision, with `dev` as
  the default for other builds.
- Replace Negroni with the standard library HTTP server.
- Harden the HTTP server with timeouts, signal handling, graceful shutdown, and
  wrapped lifecycle errors.
- Preserve complete GCS response metadata and validators while serving stored
  object encodings consistently across `GET` and `HEAD`.
- Reuse stream-copy buffers instead of allocating 32 KiB for every object.
- Reduce memory used by concurrent small uploads while retaining resumable
  16 MiB chunks for large and unknown-size objects.
- Coalesce bursts of GCS-backed readiness checks with a one-second result cache.
- Modernize the Nix build and document current flake, upload, cache-consumer,
  health-check, and localhost-first performance workflows.

### Fixed

- Abort failed streaming uploads instead of finalizing partial GCS objects.
- Evaluate read preconditions before reporting invalid or unsatisfiable ranges.
- Enforce conditional `PUT` requests atomically with GCS object generations.
- Return correctly framed empty responses when HTTP preconditions fail.
- Treat byte-range units case-insensitively and ignore unsupported range requests.
- Advertise the proxy's supported methods in `405 Method Not Allowed`
  responses.

## [0.1.0] - 2019-09-04

### Fixed

- Use `ScopeReadWrite` instead of `ScopeFullControl` for the client ([#5]).
- Write object metadata while handling `PUT`, instead of immediately afterwards
  ([#6]).

## [0.0.2] - 2019-09-02

### Added

- Log errors to the console ([#4]).

### Fixed

- Improve HTTP response statuses.

## [0.0.1]

### Added

- Initial release.

[Unreleased]: https://github.com/mtibben/nix-store-gcs-proxy/compare/fbcaae5...HEAD
[0.1.0]: https://github.com/mtibben/nix-store-gcs-proxy/compare/74152d7...fbcaae5
[0.0.2]: https://github.com/mtibben/nix-store-gcs-proxy/compare/e3b0a58...74152d7
[0.0.1]: https://github.com/mtibben/nix-store-gcs-proxy/commit/e3b0a58
[#4]: https://github.com/mtibben/nix-store-gcs-proxy/pull/4
[#5]: https://github.com/mtibben/nix-store-gcs-proxy/pull/5
[#6]: https://github.com/mtibben/nix-store-gcs-proxy/pull/6
