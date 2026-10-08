#!/usr/bin/env bash
#
# Upload a directory of built tool binaries to the team's FTP file server, the
# one http://10.0.0.106:8021 serves read-only.
#
#   upload-tool.sh <local-dir> <remote-dir>
#
# Every regular file under <local-dir> goes to the same relative path under
# <remote-dir>, which is created as needed. <remote-dir> is relative to
# TOOLS_FTP_BASE.
#
# Configuration, as CI/CD variables (the two credentials protected and masked,
# so only the default branch and tags ever see them):
#
#   FTP_USER          required.
#   FTP_PASSWORD      required.
#   TOOLS_FTP_HOST    optional, default 10.0.0.106.
#   TOOLS_FTP_PORT    optional, default 21.
#   TOOLS_FTP_BASE    optional, default /aioznode: the directory the HTTP server
#                     on :8021 serves as its root.
#
# Transport is curl, as in depin's scripts/release/artifact-store.sh: the job
# image already has it, and it speaks FTP. Plain FTP with no TLS matches that
# convention for this host, which sits on the same trusted LAN as the registry;
# the password crosses that segment in clear text.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 <local-dir> <remote-dir>" >&2
  exit 2
fi
LOCAL_DIR="${1%/}"
REMOTE_DIR="${2#/}"
REMOTE_DIR="${REMOTE_DIR%/}"

if [ ! -d "$LOCAL_DIR" ]; then
  echo "upload-tool.sh: $LOCAL_DIR is not a directory" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "upload-tool.sh: curl is not installed in this job image" >&2
  exit 1
fi

missing=""
[ -n "${FTP_USER:-}" ] || missing="${missing} FTP_USER"
[ -n "${FTP_PASSWORD:-}" ] || missing="${missing} FTP_PASSWORD"
if [ -n "$missing" ]; then
  echo "upload-tool.sh: missing required variable(s):${missing}" >&2
  echo "  Set them as protected, masked CI/CD variables of this project." >&2
  exit 1
fi

HOST="${TOOLS_FTP_HOST:-10.0.0.106}"
PORT="${TOOLS_FTP_PORT:-21}"
BASE="${TOOLS_FTP_BASE:-/aioznode}"
BASE="/${BASE#/}"
BASE="${BASE%/}"

# Names are built here from a ref slug, a short sha and GOOS/GOARCH, so they are
# URL-clean by construction. Refuse anything else rather than encode it: curl
# fails on an illegal URL halfway through, leaving a partial upload.
case "$REMOTE_DIR" in
*[!A-Za-z0-9._/-]* | *..*)
  echo "upload-tool.sh: remote dir $REMOTE_DIR has characters this script does not encode" >&2
  exit 1
  ;;
esac

# Credentials go in a curl config file, never on the command line, where every
# process on the runner could read them from ps for as long as curl runs. The
# file is created under a 077 umask before anything is written to it. Quotes
# and backslashes are escaped, because the config file's string syntax would
# otherwise end the value early and send a truncated password.
CURL_CONFIG="$(umask 077 && mktemp "${TMPDIR:-/tmp}/upload-tool-curl.XXXXXX")"
trap 'rm -f "${CURL_CONFIG:-}"' EXIT INT TERM
escape() {
  local s="${1//\\/\\\\}"
  printf '%s' "${s//\"/\\\"}"
}
printf 'user = "%s:%s"\n' "$(escape "$FTP_USER")" "$(escape "$FTP_PASSWORD")" >"$CURL_CONFIG"

# --fail makes a rejected login or a refused write a non-zero exit instead of a
# green job that uploaded nothing. The double slash after the port is what
# makes the path absolute rather than relative to the login directory.
count=0
while IFS= read -r -d '' file; do
  rel="${file#"$LOCAL_DIR"/}"
  case "$rel" in
  *[!A-Za-z0-9._/-]*)
    echo "upload-tool.sh: file name $rel has characters this script does not encode" >&2
    exit 1
    ;;
  esac
  url="ftp://${HOST}:${PORT}/${BASE}/${REMOTE_DIR}/${rel}"
  echo "upload ${rel} -> ${BASE}/${REMOTE_DIR}/${rel}"
  curl --fail --silent --show-error --ftp-create-dirs \
    --retry 3 --retry-delay 2 --connect-timeout 20 \
    --config "$CURL_CONFIG" -T "$file" "$url"
  count=$((count + 1))
done < <(find "$LOCAL_DIR" -type f -print0 | sort -z)

if [ "$count" -eq 0 ]; then
  echo "upload-tool.sh: nothing to upload in $LOCAL_DIR" >&2
  exit 1
fi
echo "uploaded ${count} file(s) to ${HOST}:${BASE}/${REMOTE_DIR}"
