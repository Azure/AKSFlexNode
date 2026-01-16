# Release v0.0.4 Instructions

## Tag Created

The git tag `v0.0.4` has been created locally on commit `c42d30f`.

## Next Steps to Trigger Release

To complete the release and trigger the automated release workflow, a maintainer with write access needs to push the tag to the remote repository:

```bash
# Push the tag to GitHub
git push origin v0.0.4
```

## What Happens After Tag Push

Once the tag is pushed, the following will occur automatically:

1. **Release Workflow Triggered**: The `.github/workflows/release.yml` workflow will be triggered by the tag push
2. **Binary Builds**: The workflow will build binaries for:
   - Linux AMD64 (`aks-flex-node-linux-amd64.tar.gz`)
   - Linux ARM64 (`aks-flex-node-linux-arm64.tar.gz`)
3. **Checksums Generated**: SHA256 checksums will be generated for all binaries
4. **GitHub Release Created**: A new release will be created with:
   - Release notes auto-generated from commits
   - All built binaries attached
   - Installation instructions
   - Checksum file

## Manual Workflow Trigger (Alternative)

Alternatively, the release workflow can be triggered manually using GitHub Actions workflow_dispatch:

1. Go to the Actions tab in the repository
2. Select the "Release" workflow
3. Click "Run workflow"
4. Enter `v0.0.4` as the tag input
5. Click "Run workflow"

## Verification

After the workflow completes, verify:
- [ ] Release v0.0.4 is visible at https://github.com/Azure/AKSFlexNode/releases
- [ ] Binaries are attached to the release
- [ ] Checksums file is present
- [ ] Release notes are populated

## Tag Information

```
Tag: v0.0.4
Commit: c42d30f0c8491fabc2e03ec8dbec005321619559
Message: Release v0.0.4
```
