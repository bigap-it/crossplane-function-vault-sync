# Crossplane Function: Vault Sync

A Crossplane Composition Function that automatically syncs Scaleway S3 credentials to HashiCorp Vault.

## What it does

This function:
1. Reads the Scaleway API Key `accessKey` from the ApiKey resource status
2. Reads the Scaleway API Key `secretKey` from the Kubernetes Secret
3. Pushes both credentials to Vault at `secret/<app-name>/s3-backup`
4. Updates the AppBackupBucket status with sync information

## Why a custom function?

The default `function-shell` doesn't include `kubectl`, `jq`, or `curl` in its container image. This custom function uses the Kubernetes Go client to access resources directly, and the standard Go HTTP client to push to Vault.

## Architecture

```
AppBackupBucket CR
    ↓
Crossplane Pipeline
    ↓
Step 1: patch-and-transform (creates Scaleway resources)
    ↓
Step 2: function-vault-sync (this function)
    ↓
    ├─→ Read ApiKey.status.atProvider.accessKey
    ├─→ Read Secret.data["attribute.secret_key"]
    ├─→ Push to Vault API
    └─→ Update AppBackupBucket.status
```

## Usage

In your Composition:

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: appbackupbucket.platform.bigap.fr
spec:
  mode: Pipeline
  pipeline:
    - step: create-scaleway-resources
      functionRef:
        name: function-patch-and-transform
      input:
        # ... your Scaleway resources

    - step: sync-to-vault
      functionRef:
        name: function-vault-sync
      input:
        apiVersion: vault.fn.bigap.fr/v1beta1
        kind: Parameters
        spec:
          vaultAddress: http://vault.vault-system:8200
          vaultMount: secret
```

## Development

### Prerequisites

- Go 1.21+
- Docker
- kubectl

### Build

```bash
# Build the binary
go build -o function-vault-sync .

# Build the Docker image
docker build -t ghcr.io/bigap-it/crossplane-function-vault-sync:v1.0.0 .

# Push to registry
docker push ghcr.io/bigap-it/crossplane-function-vault-sync:v1.0.0
```

### Install in Crossplane

```yaml
apiVersion: pkg.crossplane.io/v1beta1
kind: Function
metadata:
  name: function-vault-sync
spec:
  package: ghcr.io/bigap-it/crossplane-function-vault-sync:v1.0.0
```

## License

Proprietary - Copyright (c) 2025 BIGAP
