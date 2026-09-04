#!/bin/sh
set -e

# nginx is the front door; the Go process is PID 1 so signals reach it directly.
nginx -g "daemon on;"
exec relay
