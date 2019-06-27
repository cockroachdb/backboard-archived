#!/bin/bash
# This script is used to build the Docker image.

set -exo pipefail

# Build and save off the binary.
go build -v .
mv backboard /usr/local/bin/

# Clean up image.
rm -rf /var/lib/apt/lists/* $WORKDIR /go
