#!/bin/sh
set -eu

destination=${1:?destination root is required}
commit=${S3FS_COMMIT:?S3FS_COMMIT is required}
source_dir=/tmp/s3fs-fuse

rm -rf "$source_dir"
git clone --branch v1.95 --depth 1 \
  https://github.com/s3fs-fuse/s3fs-fuse.git "$source_dir"

actual_commit=$(git -C "$source_dir" rev-parse HEAD)
if [ "$actual_commit" != "$commit" ]; then
  printf 'unexpected s3fs v1.95 commit: expected %s, got %s\n' \
    "$commit" "$actual_commit" >&2
  exit 1
fi

cd "$source_dir"
./autogen.sh
# Debian Bookworm ships Autoconf 2.71. autoupdate modernizes the pinned
# configure.ac in-place, which would make s3fs report a dirty source tree in
# its version string even though the generated configure script is valid.
git checkout -- configure.ac
./configure --prefix=/usr
make -j"$(getconf _NPROCESSORS_ONLN)"
make DESTDIR="$destination" install
test -x "$destination/usr/bin/s3fs"
