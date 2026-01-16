#!/bin/bash
# Script to create and push a new release tag
# Usage: ./scripts/create-release.sh v0.0.4

set -e

# Check if tag version is provided
if [ $# -eq 0 ]; then
    echo "Error: No tag version provided"
    echo "Usage: $0 <tag-version>"
    echo "Example: $0 v0.0.4"
    exit 1
fi

TAG=$1

# Validate tag format (should start with 'v')
if [[ ! $TAG =~ ^v[0-9]+\.[0-9]+\.[0-9]+.*$ ]]; then
    echo "Error: Tag should follow semantic versioning format (e.g., v0.0.4)"
    exit 1
fi

# Check if we're on a clean working tree
if [[ -n $(git status -s) ]]; then
    echo "Error: Working tree is not clean. Please commit or stash your changes."
    git status -s
    exit 1
fi

# Check if tag already exists locally
if git tag -l | grep -q "^${TAG}$"; then
    echo "Warning: Tag ${TAG} already exists locally"
    read -p "Do you want to delete and recreate it? (y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        git tag -d ${TAG}
        echo "Deleted local tag ${TAG}"
    else
        echo "Aborted"
        exit 1
    fi
fi

# Get current branch
CURRENT_BRANCH=$(git rev-parse --abbrev-ref HEAD)
echo "Current branch: ${CURRENT_BRANCH}"

# Show current commit
CURRENT_COMMIT=$(git rev-parse HEAD)
echo "Current commit: ${CURRENT_COMMIT}"
echo

# Confirm with user
echo "This will:"
echo "  1. Create annotated tag '${TAG}' on commit ${CURRENT_COMMIT:0:8}"
echo "  2. Push the tag to origin"
echo "  3. Trigger the release workflow on GitHub"
echo
read -p "Do you want to continue? (y/n) " -n 1 -r
echo

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted"
    exit 1
fi

# Create annotated tag
echo "Creating tag ${TAG}..."
git tag -a ${TAG} -m "Release ${TAG}"
echo "✓ Tag ${TAG} created locally"

# Push tag to origin
echo "Pushing tag ${TAG} to origin..."
git push origin ${TAG}
echo "✓ Tag ${TAG} pushed to origin"

echo
echo "✅ Release ${TAG} has been created and pushed successfully!"
echo
echo "Next steps:"
echo "  1. Monitor the release workflow: https://github.com/Azure/AKSFlexNode/actions"
echo "  2. Once complete, verify the release: https://github.com/Azure/AKSFlexNode/releases/tag/${TAG}"
echo "  3. Test the release binaries"
echo
