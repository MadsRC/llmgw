#!/bin/sh
# SPDX-FileCopyrightText: 2025 Mads R. Havmand <mads@v42.dk>
# SPDX-License-Identifier: AGPL-3.0-only
#MISE description="Run unit tests for the API"
#MISE depends=["build:react"]

# Run Go unit tests
go test -count=1 "$@" ./...
