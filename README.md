# nix-store-gcs-proxy - An HTTP Nix store backed by Google Cloud Storage

Nix supports multiple store backends such as file, HTTP, and S3, but not
Google Cloud Storage.

This proxy exposes a Google Cloud Storage bucket as an HTTP binary cache that
Nix can read from and write to.

## Usage

The proxy uses [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials)
to access an existing Google Cloud Storage bucket. For local use, authenticate
with:

```sh
gcloud auth application-default login
```

Alternatively, set `GOOGLE_APPLICATION_CREDENTIALS` to a service account
credential file.

### Run the proxy

From a checkout of this repository:

```sh
nix run . -- \
  --bucket-name my-cache-bucket \
  --addr localhost:3000
```

Replace `.` with `github:mtibben/nix-store-gcs-proxy` to run the package
directly from GitHub.

### Health checks

The proxy exposes separate liveness and readiness endpoints for load balancers
and container orchestrators:

- `GET` or `HEAD /livez` returns `200 OK` when the HTTP process is responding.
  It does not contact Google Cloud Storage, so use it as a liveness or restart
  check.
- `GET` or `HEAD /readyz` returns `200 OK` when the proxy can read the
  `nix-cache-info` object from the configured bucket. This checks credentials,
  network connectivity, object access, and that the bucket has been initialized
  as a Nix binary cache. The check times out after five seconds and returns
  `503 Service Unavailable` on failure.

For example:

```sh
curl --fail http://localhost:3000/livez
curl --fail http://localhost:3000/readyz
```

Nix creates `nix-cache-info` when it first copies a path to an empty binary
cache. Until that initial upload succeeds, `/livez` can pass while `/readyz`
correctly reports that the cache is not ready.

### Create a signing key

Nix signs paths written to binary caches. Generate a private key and derive the
public key that clients will trust:

```sh
nix key generate-secret \
  --key-name cache1.example.org \
  > cache.key
chmod 600 cache.key

nix key convert-secret-to-public \
  < cache.key \
  > cache.pub
```

Keep `cache.key` private. Distribute `cache.pub` to machines that consume the
cache.

### Upload store paths

In another terminal, copy a Nix installable and its closure through the proxy:

```sh
nix copy \
  --to "http://localhost:3000?secret-key=$PWD/cache.key" \
  nixpkgs#hello
```

The `secret-key` setting is the local path to the signing key. Quote the store
URL so shells do not interpret `?` as a wildcard.

### Use the cache

On a separate client, add the proxy as a substituter and trust its public key
for a single command:

```sh
nix build nixpkgs#hello \
  --extra-substituters http://localhost:3000 \
  --extra-trusted-public-keys "$(cat cache.pub)"
```

To configure the client persistently, add the following to `nix.conf`, replacing
the example public key with the contents of `cache.pub`:

```ini
extra-substituters = http://localhost:3000
extra-trusted-public-keys = cache1.example.org:<public-key>
```

Multi-user Nix installations may require a trusted user to change these
settings. Use HTTPS when exposing the proxy outside localhost.

## Development

The Nix flake provides Go and golangci-lint:

```sh
nix develop
go test ./...
golangci-lint run
```

Build and check the package with:

```sh
nix build
nix flake check
```

## TODO

* Section that explains how to setup GCS with the LB CDN.

## License

This work is licensed under the Apache License 2.0.
See [LICENSE](LICENSE) for more details.

## Sponsors

This work has been sponsored by [Digital Asset](https://digitalasset.com) and [Tweag I/O](https://tweag.io).

[![Digital Asset](https://avatars1.githubusercontent.com/u/9829909?s=200&v=4)](http://digitalasset.com)
[![Tweag I/O](https://avatars1.githubusercontent.com/u/6057932?s=200&v=4)](https://tweag.io)

This repository is maintained by [Tweag I/O](http://tweag.io)

Have questions? Need help? Tweet at
[@tweagio](http://twitter.com/tweagio).
