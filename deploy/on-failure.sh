#!/bin/bash
set -euo pipefail

UNIT_NAME="${1:-picoclaw.service}"
HOSTNAME="${2:-$(hostname)}"
TOKEN_FILE="/etc/picoclaw/telegram-alert-token"
CHATID_FILE="/etc/picoclaw/telegram-alert-chatid"

if [[ ! -f "$TOKEN_FILE" ]]; then
    echo "Error: Token file not found: $TOKEN_FILE" >&2
    exit 1
fi

if [[ ! -f "$CHATID_FILE" ]]; then
    echo "Error: Chat ID file not found: $CHATID_FILE" >&2
    exit 1
fi

TOKEN=$(cat "$TOKEN_FILE")
CHAT_ID=$(cat "$CHATID_FILE")

if [[ -z "$TOKEN" ]]; then
    echo "Error: Token is empty" >&2
    exit 1
fi

if [[ -z "$CHAT_ID" ]]; then
    echo "Error: Chat ID is empty" >&2
    exit 1
fi

TIMESTAMP=$(date -u '+%Y-%m-%d %H:%M:%S UTC')
MESSAGE="⚠️ Alert on $HOSTNAME: $UNIT_NAME entered failed state at $TIMESTAMP. Check: journalctl -u $UNIT_NAME -n 50"

curl --silent --show-error --max-time 15 \
    "https://api.telegram.org/bot${TOKEN}/sendMessage" \
    -d "chat_id=$CHAT_ID" \
    -d "text=$MESSAGE"

exit 0
