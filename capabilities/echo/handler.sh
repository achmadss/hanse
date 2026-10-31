#!/bin/sh
# echo capability handler, hanse.handler/v1.
# Read the whole request from stdin, write one response to stdout, and use
# stderr for log lines only.
contract=hanse.handler/v1
req=$(cat)

field() { # field <name>: the first string value of one envelope field
	printf '%s' "$req" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p"
}

log() { printf '%s\n' "$*" >&2; }

reply_ok() { # reply_ok <result-json>
	printf '{"contract":"%s","status":"ok","result":%s}\n' "$contract" "$1"
	exit 0
}

reply_error() { # reply_error <code> <message>
	printf '{"contract":"%s","status":"error","error":{"code":"%s","message":"%s"}}\n' \
		"$contract" "$1" "$2"
	exit 1
}

kind=$(field kind)
operation=$(field operation)

if [ "$kind" = "options" ]; then
	if [ "$(field field)" != "seconds" ]; then
		reply_error unknown_field "no options for $(field field)"
	fi
	reply_ok '{"options":[{"value":5,"label":"5 seconds"},{"value":60,"label":"1 minute"}]}'
fi

if [ "$operation" = "say" ]; then
	# The raw JSON string token keeps every escape sequence valid when it
	# is embedded in the response.
	message=$(printf '%s' "$req" | sed -n 's/.*"message"[[:space:]]*:[[:space:]]*\("\([^"\\]*\(\\.[^"\\]*\)*\)"\).*/\1/p')
	if [ -z "$message" ]; then
		reply_error missing_argument "the message argument is required"
	fi
	log "say: $message"
	reply_ok "{\"said\":$message}"
fi

if [ "$operation" = "sleep" ]; then
	seconds=$(printf '%s' "$req" | sed -n 's/.*"seconds"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')
	case "$seconds" in
	'' | *[!0-9]*)
		reply_error invalid_arguments "the seconds argument must be an integer"
		;;
	esac
	term=0
	trap 'term=1' TERM
	stop_requested() {
		[ "$term" = 1 ] || { [ -n "${HANSE_CANCEL_FILE:-}" ] && [ -e "$HANSE_CANCEL_FILE" ]; }
	}
	i=1
	while [ "$i" -le "$seconds" ]; do
		if stop_requested; then
			log "stop requested, stopping at $((i - 1)) of $seconds"
			printf '{"contract":"%s","status":"cancelled","result":{"slept":%s}}\n' \
				"$contract" "$((i - 1))"
			exit 0
		fi
		log "sleep $i of $seconds"
		# Run sleep in the background so that the TERM trap runs at once.
		sleep 1 &
		wait $! || true
		i=$((i + 1))
	done
	reply_ok "{\"slept\":$seconds}"
fi

reply_error unknown_operation "unknown operation: $operation"
