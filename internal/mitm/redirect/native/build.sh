#!/bin/bash
# Build dns_redirect shared library for the current platform.
# Usage: ./build.sh [output_dir]
#
# The output file is placed in output_dir (default: current directory).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT_DIR="${1:-.}"
SRC="${SCRIPT_DIR}/dns_redirect.c"

mkdir -p "${OUTPUT_DIR}"

OS="$(uname -s)"
ARCH="$(uname -m)"

case "${OS}" in
    Linux)
        OUTPUT="${OUTPUT_DIR}/dns_redirect.so"
        echo "Building for Linux (${ARCH})..."
        gcc -shared -fPIC -o "${OUTPUT}" "${SRC}" \
            -ldl -Wall -Wextra -O2
        echo "Built: ${OUTPUT}"
        ;;
    Darwin)
        OUTPUT="${OUTPUT_DIR}/dns_redirect.dylib"
        echo "Building for macOS (${ARCH})..."
        # On macOS, use -dynamiclib instead of -shared
        clang -dynamiclib -o "${OUTPUT}" "${SRC}" \
            -Wall -Wextra -O2
        echo "Built: ${OUTPUT}"
        ;;
    MINGW*|MSYS*|CYGWIN*)
        OUTPUT="${OUTPUT_DIR}/dns_redirect.dll"
        echo "Building for Windows (${ARCH})..."
        gcc -shared -o "${OUTPUT}" "${SRC}" \
            -lws2_32 -Wall -Wextra -O2
        echo "Built: ${OUTPUT}"
        ;;
    *)
        echo "Unsupported OS: ${OS}"
        exit 1
        ;;
esac

echo "Done."
