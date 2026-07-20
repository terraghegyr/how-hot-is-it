#!/bin/sh
# how-hot-is-it agent: reads max CPU temperature and POSTs it to the server.
# Dependencies: lm-sensors (sensors), curl, awk. Nothing else.
# Run from cron: * * * * * /opt/how-hot-is-it/agent.sh

# ==== config (edit these) ====
SERVER_URL="http://192.168.1.10:8080"
MACHINE_ID="paste-id-from-ui"
# =============================

# parse_temp reads `sensors -j` JSON on stdin and prints the maximum temperature
# (one decimal). It considers every "tempN_input" field, which covers Intel
# coretemp ("Package id 0", "Core N") and AMD k10temp ("Tctl", "Tccd*"), while
# ignoring fan RPM and voltage inputs. Exits non-zero if no temperature is found.
parse_temp() {
	awk '
		match($0, /"temp[0-9]+_input"[[:space:]]*:[[:space:]]*-?[0-9]+(\.[0-9]+)?/) {
			s = substr($0, RSTART, RLENGTH)
			sub(/^.*:[[:space:]]*/, "", s)
			if (have == 0 || s + 0 > max + 0) { max = s; have = 1 }
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
