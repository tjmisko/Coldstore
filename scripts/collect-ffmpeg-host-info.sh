#!/usr/bin/env bash
# Collect the non-sensitive host information needed to plan and document a
# private FFmpeg build for Coldstore.
#
# This script is deliberately read-only:
#   - it does not use sudo;
#   - apt-get is invoked only with --simulate;
#   - media decoding is written to /dev/null; and
#   - Coldstore configuration/environment contents and journal entries are not
#     printed; media inspection is limited to codec, format, and dimensions.
#
# Usage:
#   scripts/collect-ffmpeg-host-info.sh
#   scripts/collect-ffmpeg-host-info.sh --heic /path/to/example.heic
#   scripts/collect-ffmpeg-host-info.sh --mov /path/to/example.mov
#   scripts/collect-ffmpeg-host-info.sh --heic /path/to/file.heic --mov /path/to/file.mov
#
# HEIC_TEST_FILE and MOV_TEST_FILE are used when the corresponding option is
# omitted. Redirect stdout to a file, or pipe it through tee, to save a report.

set -uo pipefail
export LC_ALL=C

readonly REPORT_VERSION="3"

heic_file=${HEIC_TEST_FILE:-}
mov_file=${MOV_TEST_FILE:-}

usage() {
	cat <<'EOF'
Usage: collect-ffmpeg-host-info.sh [--heic PATH] [--mov PATH]

Collect a read-only, non-sensitive report for planning Coldstore's private
FFmpeg build. HEIC_TEST_FILE and MOV_TEST_FILE provide optional defaults.

Options:
  --heic PATH   Test one representative HEIC/HEIF file.
  --mov PATH    Test one representative MOV file.
  -h, --help    Show this help.
EOF
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

while (($# > 0)); do
	case $1 in
		--heic)
			(($# >= 2)) || die "--heic requires a path"
			heic_file=$2
			shift 2
			;;
		--mov)
			(($# >= 2)) || die "--mov requires a path"
			mov_file=$2
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			die "unknown argument: $1"
			;;
	esac
done

rule() {
	printf '%*s\n' 78 '' | tr ' ' '='
}

section() {
	printf '\n'
	rule
	printf '%s\n' "$1"
	rule
}

print_command() {
	printf '$'
	printf ' %q' "$@"
	printf '\n'
}

# Run a command, preserve all of its output, record its status, and continue.
# Missing optional tools are therefore visible without aborting the report.
run() {
	local command_status

	printf '\n'
	print_command "$@"
	"$@" 2>&1
	command_status=$?
	printf '[exit=%d]\n' "$command_status"
	return 0
}

run_first_line() {
	local command_status
	local first_line

	printf '\n'
	print_command "$@"
	first_line=$("$@" 2>&1)
	command_status=$?
	printf '%s\n' "$first_line" | sed -n '1p'
	printf '[exit=%d]\n' "$command_status"
	return 0
}

run_filtered() {
	local title=$1
	local expression=$2
	local command_status
	local command_output
	local filtered_output
	shift 2

	printf '\n-- %s --\n' "$title"
	print_command "$@"
	printf '  filter: %s\n' "$expression"

	command_output=$("$@" 2>&1)
	command_status=$?
	filtered_output=$(printf '%s\n' "$command_output" | grep -E "$expression" || true)
	if [[ -n $filtered_output ]]; then
		printf '%s\n' "$filtered_output"
	else
		printf '[no matching lines]\n'
	fi
	printf '[command exit=%d]\n' "$command_status"
	return 0
}

# Do not echo a sample's path into the report. Tool errors may still identify
# the input by name; inspect the report before sharing it beyond the project.
run_sample() {
	local title=$1
	local command_status
	shift

	printf '\n-- %s --\n' "$title"
	"$@" 2>&1
	command_status=$?
	printf '[exit=%d]\n' "$command_status"
	return 0
}

# Optional diagnostics should not look like a media failure when their package
# is absent. The report still says exactly what was skipped.
run_optional_sample() {
	local title=$1
	local command_name=$2
	shift 2

	if command -v "$command_name" >/dev/null 2>&1; then
		run_sample "$title" "$command_name" "$@"
	else
		printf '\n-- %s --\n' "$title"
		printf '[skipped: optional command %s is not installed]\n' "$command_name"
	fi
}

# The tested thumbnail bytes go only to /dev/null. Stderr remains visible so a
# decoder, demuxer, filter, or timeout failure is actionable.
run_sample_to_null() {
	local title=$1
	local command_status
	local command_output
	shift

	printf '\n-- %s --\n' "$title"
	command_output=$({ "$@" >/dev/null; } 2>&1)
	command_status=$?
	if [[ -n $command_output ]]; then
		printf '%s\n' "$command_output"
	fi
	printf '[exit=%d]\n' "$command_status"
	return 0
}

printf 'Coldstore FFmpeg host report\n'
printf 'Collector version: %s\n' "$REPORT_VERSION"
printf 'Read-only collection; no Coldstore configuration or credential contents are printed.\n'

section "1. Operating system, CPU, memory, and build capacity"
run date -Is
run cat /etc/os-release
run uname -a
run dpkg --print-architecture
run dpkg --print-foreign-architectures
run lscpu
run getconf GNU_LIBC_VERSION
run nproc
run free -h
run /sbin/swapon --show
run df -hT / "$HOME" /tmp
run findmnt -T "$HOME" -o TARGET,SOURCE,FSTYPE,OPTIONS
run findmnt -T /opt -o TARGET,SOURCE,FSTYPE,OPTIONS
run uptime

section "2. Compiler, build tools, and Debian package availability"
run_first_line gcc --version
run gcc -dumpmachine
run_first_line make --version
run git --version
run pkg-config --version
run nasm -v
run_first_line systemctl --version

printf '\n-- Command locations --\n'
for command_name in gcc make git pkg-config nasm curl wget gpg tar xz; do
	if command_path=$(command -v "$command_name" 2>/dev/null); then
		printf '%-12s %s\n' "$command_name" "$command_path"
	else
		printf '%-12s %s\n' "$command_name" '[not installed]'
	fi
done

printf '\n-- Selected build environment --\n'
environment_found=false
for variable_name in CC CFLAGS CPPFLAGS LDFLAGS MAKEFLAGS PKG_CONFIG_PATH PATH; do
	if [[ -v $variable_name ]]; then
		printf '%s=%s\n' "$variable_name" "${!variable_name}"
		environment_found=true
	fi
done
if [[ $environment_found == false ]]; then
	printf '[none set]\n'
fi

run apt-cache policy \
	build-essential gcc make pkg-config nasm zlib1g-dev libx264-dev \
	ca-certificates curl xz-utils gnupg
run apt-get --simulate install build-essential pkg-config nasm zlib1g-dev
run apt-mark showhold

section "3. Installed FFmpeg and relevant capabilities"
run type -a ffmpeg
run type -a ffprobe
run dpkg-query -S /usr/bin/ffmpeg /usr/bin/ffprobe
run_filtered "FFmpeg version, compiler, and library ABI" \
	'^(ffmpeg version|built with|libav|libsw|libpostproc)' \
	ffmpeg -hide_banner -version
run ffmpeg -hide_banner -buildconf
run_first_line ffprobe -hide_banner -version
run_filtered "H.264 and HEVC decoders" \
	'(^|[[:space:]])(h264|hevc)([[:space:]]|$)' \
	ffmpeg -hide_banner -decoders
run_filtered "PNG, JPEG, and optional H.264 encoders" \
	'(^|[[:space:]])(png|mjpeg|libx264)([[:space:]]|$)' \
	ffmpeg -hide_banner -encoders
run_filtered "Thumbnail and scaling filters" \
	'[[:space:]](scale|thumbnail|xstack)[[:space:]]' \
	ffmpeg -hide_banner -filters
run_filtered "MOV, HEIF, and image-sequence demuxers" \
	'[[:space:]](mov|heif|image2)' \
	ffmpeg -hide_banner -demuxers

section "4. Coldstore service integration"
run systemctl is-active coldstore
run systemctl is-enabled coldstore
run systemctl --no-pager --full show coldstore \
	-p User \
	-p Group \
	-p ExecStart \
	-p WorkingDirectory \
	-p FragmentPath \
	-p DropInPaths \
	-p EnvironmentFiles \
	-p ReadWritePaths \
	-p NoExecPaths \
	-p ExecPaths \
	-p NoNewPrivileges \
	-p ProtectSystem \
	-p ProtectHome
run systemd-path search-binaries-default
run stat -c '%n owner=%U:%G mode=%a size=%s modified=%y' /usr/local/bin/coldstore
run stat -c '%n owner=%U:%G mode=%a' /etc/coldstore/coldstore.toml
if systemctl is-active --quiet coldstore; then
	printf '\n-- Offline Coldstore archive report --\n'
	printf '[skipped: daemon is active; the live catalog may have an uncheckpointed WAL]\n'
else
	run /usr/local/bin/coldstore --config /etc/coldstore/coldstore.toml --status
fi

section "5. Representative media acceptance tests"

if [[ -n $heic_file ]]; then
	if [[ -r $heic_file ]]; then
		run_sample "HEIC size" stat -c 'bytes=%s' -- "$heic_file"
		run_optional_sample "HEIC file identification" file -b -- "$heic_file"
		run_sample "HEIC non-sensitive properties" \
			exiftool -s -FileType -MIMEType -ImageWidth -ImageHeight -- "$heic_file"
		run_sample "HEIC ffprobe stream inspection" \
			ffprobe -v error -select_streams v:0 \
			-show_entries stream=codec_name,profile,pix_fmt,width,height \
			-of default=noprint_wrappers=1 -- "$heic_file"
		run_sample_to_null "HEIC Coldstore command (35-second diagnostic limit)" \
			timeout 35s ffmpeg -hide_banner -v error -i "$heic_file" \
			-vframes 1 -f image2pipe -vcodec png -
	else
		printf '\nHEIC test skipped: supplied path is not readable.\n'
	fi
else
	printf '\nHEIC test skipped: use --heic PATH or set HEIC_TEST_FILE.\n'
fi

if [[ -n $mov_file ]]; then
	if [[ -r $mov_file ]]; then
		run_sample "MOV size" stat -c 'bytes=%s' -- "$mov_file"
		run_optional_sample "MOV file identification" file -b -- "$mov_file"
		run_sample "MOV ffprobe stream inspection" \
			ffprobe -v error -select_streams v:0 \
			-show_entries stream=codec_name,codec_long_name,profile,pix_fmt,width,height \
			-of default=noprint_wrappers=1 -- "$mov_file"
		run_sample_to_null "MOV command-equivalent pipeline (35-second diagnostic limit)" \
			timeout 35s ffmpeg -hide_banner -v error -i "$mov_file" \
			-vf "thumbnail,scale='min(400\\,iw):-1'" \
			-frames:v 1 -f image2pipe -vcodec mjpeg -
	else
		printf '\nMOV test skipped: supplied path is not readable.\n'
	fi
else
	printf '\nMOV test skipped: use --mov PATH or set MOV_TEST_FILE.\n'
fi

section "End of report"
printf 'Exit status 0 means success; timeout(1) reports 124 when a media test expires.\n'
printf 'Media diagnostics allow 35 seconds; Coldstore itself allows 30 seconds per tool process.\n'
printf 'Review file names in tool error messages before sharing this report publicly.\n'
