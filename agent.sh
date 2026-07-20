#!/bin/sh
# how-hot-is-it agent: reads max CPU temperature and POSTs it to the server.
# Dependencies: lm-sensors (sensors), curl, awk. Nothing else.
# Run from cron every 30s (cron's floor is 1 min, so use two offset entries):
#   * * * * * /opt/how-hot-is-it/agent.sh
#   * * * * * sleep 30; /opt/how-hot-is-it/agent.sh

# ==== config (edit these) ====
SERVER_URL="http://192.168.1.10:8080"
MACHINE_ID="paste-id-from-ui"
# =============================

# parse_temp reads `sensors -j` JSON on stdin and prints the maximum CPU
# temperature (one decimal). It only considers "tempN_input" fields that belong
# to a CPU-temperature chip — Intel coretemp ("Package id 0", "Core N") and AMD
# k10temp/k8temp/zenpower ("Tctl", "Tccd*") — so unrelated sensors (chipset
# pch_*, acpitz, nvme drives, motherboard super-I/O fans/voltages) never win.
# Chips are the depth-1 keys of the JSON, tracked here by brace counting.
# Exits non-zero if no CPU temperature is found.
parse_temp() {
	awk '
		BEGIN { depth = 0; incpu = 0; have = 0 }
		{
			# A key opening an object at the chip level (depth 1) names a chip;
			# only CPU-temperature drivers count.
			if (depth == 1 && match($0, /"[^"]+"[[:space:]]*:[[:space:]]*[{]/)) {
				chip = substr($0, RSTART + 1)
				sub(/".*/, "", chip)
				incpu = (chip ~ /^(coretemp|k10temp|k8temp|zenpower)/)
			}
			if (incpu && match($0, /"temp[0-9]+_input"[[:space:]]*:[[:space:]]*-?[0-9]+(\.[0-9]+)?/)) {
				s = substr($0, RSTART, RLENGTH)
				sub(/^.*:[[:space:]]*/, "", s)
				if (have == 0 || s + 0 > max + 0) { max = s; have = 1 }
			}
			# Track nesting depth so the next chip key is detected correctly.
			line = $0
			depth += gsub(/[{]/, "", line) - gsub(/[}]/, "", line)
		}
		END { if (have == 0) exit 1; printf "%.1f\n", max + 0 }
	'
}

# read_temp_sysfs is the fallback when sensors is unavailable: max of the
# thermal_zone temperatures (millidegrees) divided by 1000.
read_temp_sysfs() {
	awk '
		{ if (have == 0 || $1 + 0 > max + 0) { max = $1; have = 1 } }
		END { if (have == 0) exit 1; printf "%.1f\n", max / 1000 }
	' /sys/class/thermal/thermal_zone*/temp 2>/dev/null
}

read_temp() {
	if command -v sensors >/dev/null 2>&1; then
		t=$(sensors -j 2>/dev/null | parse_temp)
		if [ -n "$t" ]; then
			printf '%s\n' "$t"
			return 0
		fi
	fi
	read_temp_sysfs
}

main() {
	TEMP=$(read_temp)
	[ -z "$TEMP" ] && exit 1
	# curl -m 5 keeps cron from ever hanging; fail-silent on network errors so
	# a dropped sample is simply retried next minute.
	curl -fsS -m 5 -X POST "$SERVER_URL/api/report" \
		-H 'Content-Type: application/json' \
		-d "{\"machine_id\":\"$MACHINE_ID\",\"temp_c\":$TEMP}" >/dev/null 2>&1
}

# Allow tests to source this file (HHII_LIB=1) to exercise parse_temp without
# running the agent.
if [ "${HHII_LIB:-0}" != "1" ]; then
	main "$@"
fi
