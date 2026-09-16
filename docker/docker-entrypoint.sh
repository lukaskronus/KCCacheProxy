#!/bin/sh
# KCCacheProxy Go entrypoint
# Usage: ./docker-entrypoint.sh [proxy|mod:command]
exec /app/main "$@"