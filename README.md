# openvicloud — the OpenViCloud CLI

Manage the resources of your OpenViCloud tenant from a terminal: sign in on the web, create and control VMs, open an
SSH session to a VM that has no public address.

## Install

Download the binary for your machine from the [latest release](https://github.com/OpenViCloud/titan-cli/releases/latest)
(`openvicloud-<linux|darwin|windows>-<amd64|arm64>`), check it, and put it on your `PATH`:

```sh
curl -fsSLO https://github.com/OpenViCloud/titan-cli/releases/latest/download/openvicloud-linux-amd64
curl -fsSLO https://github.com/OpenViCloud/titan-cli/releases/latest/download/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS      # macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
chmod +x openvicloud-linux-amd64 && mv openvicloud-linux-amd64 ~/.local/bin/openvicloud
```

`SHA256SUMS` is signed by the OpenViCloud release key
(`BB74 B8EC F2BA A232 1296  E3BA 6200 9A5A 7C77 9F46`, [openvicloud-release.asc](openvicloud-release.asc)):

```sh
curl -fsSLO https://github.com/OpenViCloud/titan-cli/releases/latest/download/SHA256SUMS.sig
gpg --import openvicloud-release.asc
gpg --verify SHA256SUMS.sig SHA256SUMS
```

Or build it with Go 1.25 or newer:

```sh
go install github.com/OpenViCloud/titan-cli/cmd/openvicloud@latest
```

`go install` works under this path from the first release after v0.1.0; v0.1.0 still declares the repository's
old module path, so take that version's binary from the release instead.

## Sign in

```sh
openvicloud auth login     # prints a link and a code, finish on the web
openvicloud auth status
```

For CI jobs and scripts, create an API token on the portal (API tokens page) and set it as `OPENVICLOUD_TOKEN`;
no browser login is needed then.

## Commands

| Area | Commands |
|---|---|
| Account | `auth login` · `auth logout` · `auth status` · `tenant get` · `version` |
| Catalog | `cluster list` · `template list [--cluster]` · `size list` |
| VMs | `vm list` · `vm get` · `vm create` · `vm rename` · `vm start` · `vm stop` · `vm delete` · `vm ssh` |
| SSH keys | `ssh-key add` · `ssh-key list` · `ssh-key delete` |

`create`, `start`, `stop` and `delete` take `--wait` to return only once the request is done. Every command takes
`-o json`. Names or ids are accepted wherever a VM, network, template, size or SSH key is chosen.

The CLI targets the OpenViCloud staging API until production exists; `OPENVICLOUD_API_URL`, `OPENVICLOUD_AUTH_URL`
and `OPENVICLOUD_PORTAL_URL` point it elsewhere.

## Development

```sh
go vet ./...
go test ./...
go build -o openvicloud ./cmd/openvicloud
```

`api/client.gen.go` is generated from `api/openapi-v1.json`, a copy of the public API contract kept in
[OpenViCloud/titan-control-plane](https://github.com/OpenViCloud/titan-control-plane); `api/SOURCE` names the
control-plane commit it was copied from. To take a newer contract:

```sh
scripts/sync-spec.sh <control-plane commit, tag or branch>
```

CI fails when the committed client does not match the committed spec. A tag `vX.Y.Z` publishes a release
(`.github/workflows/release.yml`, `.goreleaser.yaml`) once a maintainer approves the `release` environment. The
design rules of the CLI live in titan-ops's [docs/features/cli](https://github.com/OpenViCloud/titan-ops/blob/main/docs/features/cli/design.md).

## License

[MPL-2.0](LICENSE)
