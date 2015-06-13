# common functions

# log_command logs a command, runs it, and then checks the return value.
#
# $@: the command
function log_command() {
  echo "executing: $@" >&2
  $@
  local exit_status="$?"
  if [ ${exit_status} -ne 0 ]; then
    echo "error: $@ failed with exit status ${exit_status}" >&2
    return ${exit_status}
  fi
  echo "success: $@" >&2
  return 0
}

# check_value_set checks if a named value is set.
#
# $1: name of the value
function check_value_set() {
  if [ -z "${!1}" ]; then
    echo "error: ${1} not set" >&2
    return 1
  fi
  return 0
}

# check_executable checks if an executable is on PATH, and if the
# executable's version is acceptable.
# Version strings must be of the form MAJOR.MINOR.MICRO.
# check_executable  fails if the executable is not on PATH,
# the major version is not equal, or the micro version is
# not at least, the expected values.

# $1: executable
# $2: function to parse out version string
# $3: expected major version
# $4: expected minor version
function check_executable() {
  local executable="${1}"
  local version="${2}"
  local major="${3}"
  local minor="${4}"

  if ! which "${executable}" > /dev/null; then
    echo "error: ${executable} not installed" >&2
    return 1
  fi

  local regex="([0-9]+)\.([0-9]+)\.([0-9]+)"
  if [ "$(echo "${version}" | sed -E "s/${regex}/\1/")" -ne "${major}" ]; then
    echo "error: ${executable} version ${version} must have major version ${major}" >&2
    return 1
  fi
  if [ "$(echo "${version}" | sed -E "s/${regex}/\2/")" -lt "${minor}" ]; then
    echo "error: ${executable} version ${version} must have minor version of at least ${minor}" >&2
    return 1
  fi
  echo "success: ${executable} version ${version} ok" >&2
  return 0
}

# btrfs_version returns the btfrs version in the form MAJOR.MINOR.MICRO.
function btrfs_version() {
  btrfs --version | awk '{ print $2 }' | sed "s/v//"
}

# docker_version returns the btfrs version in the form MAJOR.MINOR.MICRO.
function docker_version() {
  docker --version | awk '{ print $3 }' | sed "s/,//"
}

# check_btrfs checks if btrfs is on the PATH and the version.
function check_btrfs() {
  check_executable "btrfs" "$(btrfs_version)" "${BTRFS_MAJOR_VERSION}" "${BTRFS_MINOR_VERSION}"
}

# check_docker checks if docker is on the PATH and the version.
function check_docker() {
  check_executable "docker" "$(docker_version)" "${DOCKER_MAJOR_VERSION}" "${DOCKER_MINOR_VERSION}"
}