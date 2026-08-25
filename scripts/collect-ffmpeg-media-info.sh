#!/usr/bin/env bash
# Collect the remaining Coldstore status and representative HEIC/MOV evidence.
# The script is read-only: decoded thumbnail bytes are discarded, while tool
# errors and exit statuses remain in the report.

set -uo pipefail
export LC_ALL=C

usage() {
	cat <<'EOF'
Usage: collect-ffmpeg-media-info.sh HEIC_FILE MOV_FILE

Example:
  bash collect-ffmpeg-media-info.sh '/path/to/example.heic' '/path/to/example.mov'
EOF
}

if (($# != 2)); then
	usage >&2
	exit 2
fi

readonly HEIC_FILE=$1
readonly MOV_FILE=$2

section() {
	printf '\n==============================================================================\n'
	printf '%s\n' "$1"
	printf '==============================================================================\n'
}

run_check() {
	local title=$1
	local command_status
	shift

	printf '\n-- %s --\n' "$title"
	"$@" 2>&1
	command_status=$?
	printf '[exit=%d]\n' "$command_status"
	return 0
}

run_optional_check() {
	local title=$1
	local command_name=$2
	shift 2

	if command -v "$command_name" >/dev/null 2>&1; then
		run_check "$title" "$command_name" "$@"
	else
		printf '\n-- %s --\n' "$title"
		printf '[skipped: optional command %s is not installed]\n' "$command_name"
	fi
}

run_thumbnail_check() {
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

printf 'Coldstore FFmpeg media report\n'
printf 'Generated: %s\n' "$(date -Is)"
if ffmpeg_path=$(command -v ffmpeg 2>/dev/null); then
	printf 'FFmpeg selected: %s\n' "$ffmpeg_path"
	ffmpeg -hide_banner -version 2>&1 | sed -n '1p'
else
	printf 'FFmpeg selected: [not installed]\n'
fi
if ffprobe_path=$(command -v ffprobe 2>/dev/null); then
	printf 'ffprobe selected: %s\n' "$ffprobe_path"
else
	printf 'ffprobe selected: [not installed]\n'
fi

section "1. Coldstore status"
run_check "systemd service state" systemctl is-active coldstore
if systemctl is-active --quiet coldstore; then
	printf '\n-- Offline archive health report --\n'
	printf '[skipped: daemon is active; the live catalog may have an uncheckpointed WAL]\n'
else
	run_check "Offline archive health report" \
		/usr/local/bin/coldstore \
		--config /etc/coldstore/coldstore.toml \
		--status
fi

section "2. HEIC acceptance test"
if [[ -r $HEIC_FILE ]]; then
	run_optional_check "File identification" file -b -- "$HEIC_FILE"
	run_check "Non-sensitive HEIC properties" \
		exiftool -s -FileType -MIMEType -ImageWidth -ImageHeight -- "$HEIC_FILE"
	run_check "ffprobe stream inspection" \
		ffprobe -v error -select_streams v:0 \
		-show_entries stream=codec_name,profile,pix_fmt,width,height \
		-of default=noprint_wrappers=1 -- "$HEIC_FILE"
	run_thumbnail_check "Coldstore HEIC command (35-second diagnostic limit)" \
		timeout 35s ffmpeg -hide_banner -v error -i "$HEIC_FILE" \
		-vframes 1 -f image2pipe -vcodec png -
else
	printf '\nHEIC input is not readable.\n'
	printf '[exit=2]\n'
fi

section "3. MOV acceptance test"
if [[ -r $MOV_FILE ]]; then
	run_optional_check "File identification" file -b -- "$MOV_FILE"
	run_check "ffprobe stream inspection" \
		ffprobe -v error -select_streams v:0 \
		-show_entries stream=codec_name,codec_long_name,profile,pix_fmt,width,height \
		-of default=noprint_wrappers=1 -- "$MOV_FILE"
	run_thumbnail_check "MOV command-equivalent pipeline (35-second diagnostic limit)" \
		timeout 35s ffmpeg -hide_banner -v error -i "$MOV_FILE" \
		-vf "thumbnail,scale='min(400\\,iw):-1'" \
		-frames:v 1 -f image2pipe -vcodec mjpeg -
else
	printf '\nMOV input is not readable.\n'
	printf '[exit=2]\n'
fi

section "End of report"
printf 'Exit 0 means success; timeout reports 124. Other values are tool failures.\n'
printf 'Diagnostics allow 35 seconds; Coldstore itself allows 30 seconds per tool process.\n'
