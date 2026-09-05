#!/bin/sh
# Generates the source fixture tree. The 200 MB blob is generated, never
# committed; SHA256SUMS is written here so the specs compare against one
# recorded truth rather than recomputing on both ends.
set -eu
d=/upload
[ -f "$d/SHA256SUMS" ] && exit 0

mkdir -p "$d/tree/a/b/c"
printf 'hello sftp-relay\n' > "$d/small.txt"
head -c 209715200 /dev/urandom > "$d/big.bin"
: > "$d/empty.bin"
printf 'spaces and unicode\n' > "$d/name with spaces ü.txt"
printf 'three levels down\n' > "$d/tree/a/b/c/deep.txt"
for i in 1 2 3 4 5; do head -c 4096 /dev/urandom > "$d/tree/file$i.bin"; done

cd "$d"
find . -type f ! -name SHA256SUMS -exec sha256sum {} + | sort -k2 > SHA256SUMS
chmod -R a+rX "$d"
