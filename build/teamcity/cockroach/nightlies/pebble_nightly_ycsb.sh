#!/usr/bin/env bash

# Copyright 2022 The Cockroach Authors.
#
# Use of this software is governed by the CockroachDB Software License
# included in the /LICENSE file.

#
# This script runs the Pebble Nightly YCSB benchmarks.
#
# It is run by the Pebble Nightly YCSB TeamCity build
# configuration.

set -euo pipefail

dir="$(dirname $(dirname $(dirname $(dirname "${0}"))))"

source "$dir/teamcity-support.sh"  # For $root
source "$dir/teamcity-bazel-support.sh"  # For run_bazel

BAZEL_SUPPORT_EXTRA_DOCKER_ARGS="-e LITERAL_ARTIFACTS_DIR=$root/artifacts -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e GOOGLE_EPHEMERAL_CREDENTIALS -e TC_BUILDTYPE_ID -e TC_BUILD_BRANCH -e TC_BUILD_ID -e TC_SERVER_URL -e PEBBLE_SHA -e ROACHTEST_NAME -e ROACHTEST_SUITE" \
                               run_bazel build/teamcity/cockroach/nightlies/pebble_nightly_ycsb_impl.sh
