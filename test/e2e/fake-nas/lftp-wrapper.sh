#!/bin/sh
# A transparent shim in front of the real lftp, so the suite can slow a transfer
# down. The loopback between two containers moves 200 MB in well under a second,
# which leaves no window at all for the progress, cancel and restart-resume
# scenarios; tc is not an option because Docker Desktop's kernel ships no
# act_police module.
#
# With no /tmp/e2e-throttle file this is exec + argv, nothing else.
if [ -f /tmp/e2e-throttle ] && [ "$1" = "-f" ]; then
  rate=$(cat /tmp/e2e-throttle)
  script=$(mktemp)
  printf 'set net:limit-total-rate %s:%s\n' "$rate" "$rate" > "$script"
  cat "$2" >> "$script"
  set -- -f "$script"
fi
exec /opt/bin/lftp.real "$@"
