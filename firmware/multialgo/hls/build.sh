#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail
if [[ $# != 4 && $# != 5 ]]; then
  printf 'Usage: %s lz4|zstd NEW_BUILD_ROOT VITIS_LIBRARIES csim|synth|cosim [quick|random|entropy]\n' "$0" >&2
  exit 2
fi
cf_algorithm=$1
cf_root=$2
cf_library=$(cd -- "$3" && pwd)
cf_mode=$4
cf_cases=${5:-quick}
case $cf_algorithm in lz4|zstd) ;; *) exit 2;; esac
case $cf_mode in csim|synth|cosim) ;; *) exit 2;; esac
case $cf_cases in quick|random|entropy) ;; *) exit 2;; esac
if [[ -e $cf_root ]]; then
  printf 'Build directory already exists: %s\n' "$cf_root" >&2
  exit 2
fi
cf_source=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cf_sdk=$(cd -- "$cf_source/../../.." && pwd)
mkdir -p -- "$cf_root/source"
cf_root=$(cd -- "$cf_root" && pwd)
cp -- "$cf_source/build.tcl" "$cf_source/compress.cpp" "$cf_source/test.cpp" \
  "$cf_source"/*.patch "$cf_root/source/"
CGO_ENABLED=0 GOWORK=off go -C "$cf_sdk" build -trimpath \
  -o "$cf_root/verify-hls" ./examples/verify-hls
go version -m "$cf_root/verify-hls" >"$cf_root/verify-hls-go.txt"
python3 - "$cf_root" "$cf_library" "$cf_algorithm" "$cf_mode" "$cf_cases" <<'PY'
import hashlib,json,pathlib,sys
root,library=map(pathlib.Path,sys.argv[1:3]);files={}
for p in sorted((root/'source').rglob('*')):
    if p.is_file():files['source/'+str(p.relative_to(root/'source'))]=hashlib.sha256(p.read_bytes()).hexdigest()
comp=library/'data_compression'
if not comp.is_dir():comp=library/'comp'
for name,directory in [('compression',comp/'L1/include'),('security',library/'security/L1/include')]:
    for p in sorted(directory.rglob('*')):
        if p.is_file():files[name+'/'+str(p.relative_to(directory))]=hashlib.sha256(p.read_bytes()).hexdigest()
files['verify-hls']=hashlib.sha256((root/'verify-hls').read_bytes()).hexdigest()
(root/'source-inputs.json').write_text(json.dumps(dict(algorithm=sys.argv[3],mode=sys.argv[4],cases=sys.argv[5],files=files),indent=2)+'\n')
PY
export CF_HLS_ALGORITHM=$cf_algorithm CF_HLS_BUILD=$cf_root
export CF_HLS_LIBRARY=$cf_library CF_HLS_MODE=$cf_mode CF_HLS_CASES=$cf_cases
export CF_HLS_VERIFY=$cf_root/verify-hls
cf_vitis=${VITIS_HLS_BIN:-vitis-run}
cd -- "$cf_root"
exec "$cf_vitis" --mode hls --tcl "$cf_root/source/build.tcl"
