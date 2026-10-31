#!/bin/sh
# testkit capability handler, hanse.handler/v1.
#
# The operations only write output, wait, or fail on request. Read the whole
# request from stdin, write one response to stdout, and use stderr for log
# lines only. This handler needs sh and jq; handler.ps1 is the Windows
# handler.
contract=hanse.handler/v1
req=$(cat)

# Report a missing jq with a handler error, written without jq, so a minimal
# host fails cleanly instead of with a shell error.
if ! command -v jq >/dev/null 2>&1; then
	printf '{"contract":"%s","status":"error","error":{"code":"missing_dependency","message":"the testkit handler needs jq on Linux and macOS"}}\n' "$contract"
	exit 1
fi

# json_field <jq expression> prints one value of the envelope.
json_field() { printf '%s' "$req" | jq -r "$1"; }

# arg <name> prints one argument value, or nothing when it is absent.
arg() { printf '%s' "$req" | jq -r --arg name "$1" '.arguments[$name] // empty'; }

# arg_int <name> <default> prints an integer argument, or the default.
arg_int() {
	value=$(arg "$1")
	case "$value" in
	'' | *[!0-9]*) printf '%s' "$2" ;;
	*) printf '%s' "$value" ;;
	esac
}

log() { printf '%s\n' "$*" >&2; }

reply_ok() { # reply_ok <result-json> [summary]
	jq -cn --arg c "$contract" --argjson r "$1" --arg s "${2:-}" \
		'{contract:$c,status:"ok",result:$r} + (if $s == "" then {} else {summary:$s} end)'
	exit 0
}

reply_cancelled() { # reply_cancelled <result-json> [summary]
	jq -cn --arg c "$contract" --argjson r "$1" --arg s "${2:-}" \
		'{contract:$c,status:"cancelled",result:$r} + (if $s == "" then {} else {summary:$s} end)'
	exit 0
}

reply_error() { # reply_error <code> <message> [details-json]
	jq -cn --arg c "$contract" --arg code "$1" --arg msg "$2" --argjson d "${3:-null}" \
		'{contract:$c,status:"error",error:({code:$code,message:$msg} + (if $d == null then {} else {details:$d} end))}'
	exit 1
}

# stop_requested reports whether the agent asked the handler to stop: it
# sends SIGTERM on POSIX systems and creates the file at HANSE_CANCEL_FILE
# on every platform.
term=0
trap 'term=1' TERM
stop_requested() {
	[ "$term" = 1 ] || { [ -n "${HANSE_CANCEL_FILE:-}" ] && [ -e "$HANSE_CANCEL_FILE" ]; }
}

# An option provider call answers for the `choice` argument of testkit.choose.
if [ "$(json_field .kind)" = options ]; then
	operation=$(json_field '.operation // empty')
	option_field=$(json_field '.field // empty')
	if [ "$operation" != "choose" ] || [ "$option_field" != "choice" ]; then
		reply_error unknown_field "no options for $operation.$option_field"
	fi
	reply_ok '{"options":[{"value":"alpha","label":"Alpha"},{"value":"beta","label":"Beta","description":"The second value"},{"value":"gamma","label":"Gamma"}]}'
fi

# flood_logs <bytes> writes about that many bytes of stderr log lines.
flood_logs() {
	line=$(printf '%*s' 1023 '' | tr ' ' 'x')
	written=0
	while [ "$written" -lt "$1" ]; do
		log "flood: $line"
		written=$((written + 1029))
	done
	log "flood: wrote $written bytes of logs"
}

case "$(json_field .operation)" in
say)
	# The target node is optional: it proves that the envelope holds a node
	# an argument refers to, and the result reports it back.
	target_id=$(arg target)
	target_json=null
	if [ -n "$target_id" ]; then
		target_json=$(printf '%s' "$req" | jq -c --arg id "$target_id" '.nodes[$id] // empty')
		if [ -z "$target_json" ]; then
			reply_error unknown_node "the target argument names no node of this request"
		fi
	fi
	executor=$(printf '%s' "$req" | jq -c '.executor | {id,os,arch}')
	text=$(arg text)
	[ -z "$text" ] && text="(no text)"
	log "say: $text"
	reply_ok "$(printf '%s' "$req" | jq -c --argjson target "$target_json" --argjson executor "$executor" \
		'{arguments:.arguments,executor:$executor,target:$target}')" "said the arguments back"
	;;

long)
	seconds=$(arg_int seconds 5)
	log "long: running for $seconds seconds"
	i=1
	while [ "$i" -le "$seconds" ]; do
		if stop_requested; then
			log "long: stop requested at $((i - 1)) of $seconds"
			reply_cancelled "{\"seconds\":$((i - 1))}" "stopped after $((i - 1)) seconds"
		fi
		log "long: second $i of $seconds"
		# Run sleep in the background so that the TERM trap runs at once.
		sleep 1 &
		wait $! || true
		i=$((i + 1))
	done
	reply_ok "{\"seconds\":$seconds}" "ran for $seconds seconds"
	;;

background)
	seconds=$(arg_int seconds 300)
	interval=$(arg_int interval_seconds 60)
	log "background: running for $seconds seconds, one line every $interval seconds"
	elapsed=0
	tick=0
	while [ "$elapsed" -lt "$seconds" ]; do
		if stop_requested; then
			log "background: stop requested after $elapsed of $seconds seconds"
			reply_cancelled "{\"seconds\":$elapsed,\"ticks\":$tick}" "stopped after $elapsed seconds"
		fi
		if [ "$((elapsed % interval))" -eq 0 ]; then
			tick=$((tick + 1))
			log "background: tick $tick after $elapsed of $seconds seconds"
		fi
		sleep 1 &
		wait $! || true
		elapsed=$((elapsed + 1))
	done
	reply_ok "{\"seconds\":$seconds,\"ticks\":$tick}" "ran for $seconds seconds"
	;;

fail)
	code=$(arg error_code)
	if [ -n "$code" ]; then
		message=$(arg message)
		[ -z "$message" ] && message="the testkit handler failed on request"
		details=$(printf '%s' "$req" | jq -c '.arguments.details // null')
		log "fail: handler error $code"
		reply_error "$code" "$message" "$details"
	fi
	exit_code=$(arg_int exit_code 1)
	log "fail: exiting with code $exit_code"
	printf '{"contract":"%s","status":"ok","result":{"exit_code":%s}}\n' "$contract" "$exit_code"
	exit "$exit_code"
	;;

hang)
	seconds=$(arg_int seconds 600)
	# Ignore the stop request on purpose: this operation tests the stop
	# grace period and the kill. Log first, so a test sees the handler
	# running before it cancels.
	log "hang: ignoring stop requests for $seconds seconds"
	trap '' TERM
	elapsed=0
	while [ "$elapsed" -lt "$seconds" ]; do
		sleep 1
		elapsed=$((elapsed + 1))
		if [ "$((elapsed % 10))" -eq 0 ]; then
			log "hang: still running after $elapsed seconds"
		fi
	done
	reply_ok "{\"seconds\":$seconds}" "hung for $seconds seconds"
	;;

timeout)
	seconds=$(arg_int seconds 30)
	# Ignore the stop request: a cancelled response would end the run as
	# cancelled, and this operation tests the manifest deadline.
	log "timeout: running for $seconds seconds, beyond the manifest timeout"
	trap '' TERM
	elapsed=0
	while [ "$elapsed" -lt "$seconds" ]; do
		sleep 1
		elapsed=$((elapsed + 1))
		if [ "$((elapsed % 5))" -eq 0 ]; then
			log "timeout: still running after $elapsed seconds"
		fi
	done
	reply_ok "{\"seconds\":$seconds}" "finished after $seconds seconds"
	;;

flood)
	mode=$(arg mode)
	[ -z "$mode" ] && mode=both
	kilobytes=$(arg_int kilobytes 12288)
	bytes=$((kilobytes * 1024))
	case "$mode" in
	stderr)
		flood_logs "$bytes"
		reply_ok "{\"logs_bytes\":$bytes}" "wrote $bytes bytes of logs"
		;;
	stdout | both)
		[ "$mode" = both ] && flood_logs "$bytes"
		# One response larger than the response limit; the padding is only
		# 'x', so no JSON escaping is needed.
		pad=$(printf '%*s' "$bytes" '' | tr ' ' 'x')
		printf '{"contract":"%s","status":"ok","result":{"padding":"%s"}}\n' \
			"$contract" "$pad"
		exit 0
		;;
	*)
		reply_error invalid_arguments "mode must be stdout, stderr, or both"
		;;
	esac
	;;

choose)
	choice=$(arg choice)
	case "$choice" in
	alpha | beta | gamma) known=true ;;
	*) known=false ;;
	esac
	log "choose: $choice"
	reply_ok "$(jq -cn --arg v "$choice" --argjson known "$known" \
		'{choice:$v,known:$known}')" "chose $choice"
	;;

privileged)
	env_value=${HANSE_PRIVILEGED:-0}
	admin=false
	if [ "$(id -u)" = 0 ]; then
		admin=true
	fi
	user=$(id -un)
	# Report the lowercase Go name of the OS, as handler.ps1 does, so a
	# result reads the same on every platform: linux, darwin, or windows.
	os_name=$(uname -s | tr '[:upper:]' '[:lower:]')
	log "privileged: HANSE_PRIVILEGED=$env_value admin=$admin user=$user"
	reply_ok "$(jq -cn --arg e "$env_value" --argjson a "$admin" --arg u "$user" --arg o "$os_name" \
		'{hanse_privileged:$e,privileged:($e == "1"),admin:$a,user:$u,os:$o}')" \
		"privileged=$env_value admin=$admin"
	;;

*)
	reply_error unknown_operation "unknown operation: $(json_field .operation)"
	;;
esac
